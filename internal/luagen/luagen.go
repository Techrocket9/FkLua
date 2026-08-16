// Package luagen emits Lua 5.2 source from resolved IR.
//
// The lowerings here are measured, not assumed. Three of them are the opposite
// of what benchmarking on a modern Lua suggests, because Factorio runs 5.2.1
// with no integer subtype: wrapping uses %, shifts avoid math.floor, and bit32
// is the slowest option for nearly everything. See the codegen table in
// CLAUDE.md and bench/baselines/probe-2.0.77.json before changing any of them.
//
// Two invariants constrain every function this package writes:
//
//	Invariant A -- an i32 is an unsigned integral double in [0, 2^32).
//	Invariant B -- no `local` statement appears after the prologue.
package luagen

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
	luart "github.com/Techrocket9/fklua/runtime"
)

// prelude is the hand-written Lua runtime, inlined at the top of every chunk.
var prelude = luart.Prelude()

const (
	wrapMod = "4294967296.0" // 2^32
	signMin = "2147483648.0" // 2^31
	maxU32  = "4294967295.0" // 2^32 - 1
)

type builder struct {
	sb     strings.Builder
	indent int
	opts   Options
	// opt is the level, hoisted out of opts because every lowering asks.
	opt analysis.Level
	// w is the range analysis for the function being emitted, or nil at
	// -opt=0. Nothing dereferences it without going through its guarded
	// accessors, so "the optimizer is off" and "there is no analysis" are the
	// same state rather than two.
	w *analysis.Wrap
	// fr is typed-slot promotion for the function being emitted, or nil.
	fr *analysis.Frame
	// al is the congruence (alignment) analysis for the function being emitted,
	// or nil below the level that has a use for it. Guarded accessors again, so
	// nil means "ask nothing, assume nothing".
	al *analysis.Align
	// cl maps a loop header step to the counted loop starting there, and clEnd
	// maps that loop's closing step back to the same record. Two maps because
	// the emitter meets the two ends far apart and has to recognise each on
	// sight; both are empty below the level that lowers them.
	cl    map[int]*analysis.Counted
	clEnd map[int]*analysis.Counted
	// clDrop is every step the `for` subsumes, across all counted loops in the
	// function.
	clDrop map[int]bool
	// lg maps a loop header to the entry guard that covers its accesses, and
	// lgAccess maps each covered access step back to that guard.
	lg       map[int]*analysis.LoopGuard
	lgAccess map[int]*analysis.LoopGuard
	// ga is the module-wide global congruence, solved once before any function
	// is emitted because a global.set in one function constrains a global.get in
	// every other.
	ga analysis.GlobalAlign
	// memMax is the memory's declared ceiling in bytes, cached because the
	// memory.grow lowering prints it as a numeral. It used to be the chunk
	// local MEMMAX, whose one reader was that same site; the slot it freed is
	// what SHBOUND spends. Zero when the module has no memory.
	memMax uint64
	// up maps a function index to the chunk-level name holding it, for the
	// callees upvalue promotion picked.
	up map[uint32]string
	// plans holds every function's spill plan, computed before emission because
	// whether the chunk declares FS and FP at all depends on all of them.
	plans map[uint32]*analysis.Spill
	// sp is the frame-stack spill plan for the function being emitted, or nil
	// when everything fits in Lua locals.
	sp *analysis.Spill
	// spilled records whether ANY function in the module spills, which is what
	// decides whether the chunk declares FS and FP and whether exports are
	// wrapped to reset FP.
	spilled bool
}

// callName is how a direct call to fi is written: an upvalue when promotion
// picked it, and the F table otherwise.
func (b *builder) callName(fi uint32) string {
	if n, ok := b.up[fi]; ok {
		return n
	}
	return fmt.Sprintf("F[%d]", fi)
}

// exact reports whether NaN bits must be preserved, which routes every float
// operation through a helper instead of a plain Lua operator.
func (b *builder) exact() bool { return b.opts.NaN == NaNExact }

func (b *builder) line(format string, args ...any) {
	for i := 0; i < b.indent; i++ {
		b.sb.WriteString("  ")
	}
	fmt.Fprintf(&b.sb, format, args...)
	b.sb.WriteByte('\n')
}

func (b *builder) blank() { b.sb.WriteByte('\n') }

// slotName is the Lua identifier for a slot. Params and locals and stack slots
// share one flat namespace, which is what makes the slot map a pure function of
// stack depth.
//
// A spilled slot is named FS[base + k] instead: the frame stack is what lets a
// function needing more than Lua's 200 locals compile at all. Every slot name in
// the emitter goes through here, so the spill is invisible to every lowering.
func (b *builder) slotName(s ir.Slot) string {
	if i, ok := b.sp.At(s); ok {
		return fmt.Sprintf("FS[fb+%d]", i)
	}
	return localName(s)
}

// localName is the unspilled spelling of a slot, and the whole `v%d` family.
// Named rather than inlined so the namespace audit can enumerate it -- see the
// note on guardName for why two families sharing a spelling is a miscompile.
func localName(s ir.Slot) string { return "v" + strconv.Itoa(int(s)) }

// slotNames names every Lua local a value of type t occupies, starting at base.
// An i64 is a (lo, hi) pair and so yields two names; everything else yields one.
func (b *builder) slotNames(base ir.Slot, t wasm.ValType) string {
	if t.Slots() == 1 {
		return b.slotName(base)
	}
	parts := make([]string, t.Slots())
	for i := range parts {
		parts[i] = b.slotName(base + ir.Slot(i))
	}
	return strings.Join(parts, ", ")
}

// signature renders a function's type for the comment above it, so generated
// Lua stays readable when someone is debugging a miscompile.
func signature(f *ir.Func) string {
	ps := make([]string, len(f.Params))
	for i, p := range f.Params {
		ps[i] = p.String()
	}
	rs := make([]string, len(f.Results))
	for i, r := range f.Results {
		rs[i] = r.String()
	}
	return fmt.Sprintf("(%s) -> (%s)", strings.Join(ps, ", "), strings.Join(rs, ", "))
}

// luaString renders a byte string as a Lua literal.
//
// Go's %q cannot be used: it emits \u escapes for non-ASCII, which Lua does not
// understand at all ("invalid escape sequence near '\u'"). wasm names are
// arbitrary UTF-8 and data segments are arbitrary bytes, so everything outside
// printable ASCII goes out as a decimal \ddd escape, which every Lua accepts.
func luaString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c >= 0x20 && c < 0x7F:
			b.WriteByte(c)
		default:
			// Always three digits, so a following digit cannot extend the escape.
			fmt.Fprintf(&b, "\\%03d", c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// u32 formats an unsigned 32-bit value as an exact Lua number literal. Values
// below 2^53 are exact in a double, so no hex-float escape is needed here --
// unlike float constants, which must use hex form.
func u32(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

// EmitModule renders a module with the default options.
func EmitModule(m *ir.Module) (string, error) {
	return EmitModuleWith(m, Options{})
}

// EmitModuleWith renders a whole module as a standalone Lua chunk that returns
// a table of its exports.
func EmitModuleWith(m *ir.Module, opts Options) (string, error) {
	b := &builder{opts: opts, opt: opts.Opt, plans: map[uint32]*analysis.Spill{}}
	for _, f := range m.Funcs {
		if f.Unsupported != nil {
			continue
		}
		if sp := analysis.Plan(f, ir.MaxSlots); sp.Active() {
			b.plans[f.Index] = sp
			b.spilled = true
		} else if f.NumSlots+1 > ir.MaxSlots {
			return "", &ir.TooManySlotsError{
				Func: f.Name, Needed: f.NumSlots, Max: ir.MaxSlots,
				Params: len(f.Params), Locals: len(f.Locals), MaxStack: f.MaxStack,
			}
		}
	}
	// The congruence of every module global, solved once for the whole module
	// because a global.set anywhere constrains a global.get everywhere. Only at
	// the level that consumes it: nothing below -opt=3 asks, and asking would
	// cost a sweep per function per round for an answer nobody reads.
	if b.inlineLoads() {
		b.ga = analysis.Globals(m)
	}
	b.line("-- Generated by fklua. Do not edit.")
	b.line("--")
	b.line("-- i32 values are UNSIGNED doubles in [0, 2^32) throughout (Invariant A).")
	b.blank()
	b.sb.WriteString(prelude)
	b.blank()
	b.line("local F = {}")
	if b.opts.Fuel > 0 {
		// One chunk-level local, against a budget the prelude has already spent
		// most of. It is an upvalue rather than a parameter threaded through
		// every call because threading it would change every signature, and a
		// guest's call graph is not ours to reshape.
		b.line("local FUEL = %d", b.opts.Fuel)
	}
	emitFrameStack(b)
	emitImports(b, m)
	if err := emitModuleState(b, m); err != nil {
		return "", err
	}
	emitBranchTables(b, m)
	emitUpvalueDecls(b, m)
	b.blank()

	b.line("-- NaN mode: %s", b.opts.NaN)
	for _, f := range m.Funcs {
		if err := emitFunc(b, f); err != nil {
			return "", err
		}
		b.blank()
	}

	emitUpvalueBindings(b, m)

	if m.Source != nil && m.Source.Start >= 0 {
		// The start function runs once at instantiation, after every definition
		// exists. Emitting it here rather than not at all is the difference
		// between a guest that initialises and one that silently does not.
		b.line("-- start section")
		b.line("F[%d]()", m.Source.Start)
		b.blank()
	}
	b.line("local exports = {}")
	// What an entry point has to reset before the guest runs.
	//
	// FP: a trap unwinds straight past the epilogue that would have restored
	// it, so without this the frame stack creeps upward by one frame per trap,
	// for the life of the game session -- a slow leak whose symptom appears
	// nowhere near its cause.
	//
	// FUEL: the budget is PER ENTRY CALL, not per session. A mod that gets one
	// budget for the whole game would run fine for an hour and then start
	// trapping in a handler that had not changed, which is worse than not
	// having the check.
	var reset string
	if b.spilled {
		reset += "FP = 0 "
	}
	if b.opts.Fuel > 0 {
		reset += fmt.Sprintf("FUEL = %d ", b.opts.Fuel)
	}
	for _, e := range m.Exports {
		if reset != "" {
			b.line("exports[%s] = function(...) %sreturn F[%d](...) end",
				luaString(e.Name), reset, e.FuncIndex)
			continue
		}
		b.line("exports[%s] = F[%d]", luaString(e.Name), e.FuncIndex)
	}
	b.blank()
	// rt exposes the few runtime helpers a host needs to inspect guest values.
	// Float bit patterns are the practical case: without them a host cannot
	// distinguish -0.0 from +0.0, or read a NaN payload.
	// In exact mode these must be the BOXING variants, or a host inspecting a
	// returned NaN would hit a table where it expected a number.
	x := b.pfx()
	b.line("return { funcs = F, exports = exports,")
	if m.Source != nil && m.Source.Memory.Has {
		// Linear memory has to be reachable from outside, or a host import can
		// be handed a (pointer, length) and have no way to follow it. read_string
		// is a closure rather than a plain function so it captures MEMSIZE by
		// reference and stays correct across memory.grow.
		// mem is a SNAPSHOT of the table as it is now. It stays correct for a
		// guest that never adopts one, which is every M4 guest, and goes stale
		// the moment persistence rebinds MEM -- use persist.memory() for a live
		// answer.
		b.line("  mem = MEM,")
		b.line("  read_string = function(p, n) return fk_str(MEM, MEMSIZE, p, n) end,")
		// memio is what the host-call ABI marshals through. Closures rather
		// than the raw helpers, so they capture MEM and MEMSIZE BY REFERENCE
		// and stay correct across memory.grow and across a persistence adopt --
		// the same reason read_string is a closure.
		//
		// Bounds and traps are the prelude's, unchanged: a host writing past
		// the guest's memory gets the guest's own out-of-bounds trap rather
		// than silently extending the word table.
		b.line("  memio = {")
		b.line("    ld8 = function(a) return ld8(MEM, MEMSIZE, a) end,")
		b.line("    ld16 = function(a) return ld16(MEM, MEMSIZE, a) end,")
		b.line("    ld32 = function(a) return ld32(MEM, MEMSIZE, a) end,")
		b.line("    ldf32 = function(a) return %sbits_to_f32(ld32(MEM, MEMSIZE, a)) end,", x)
		b.line("    ldf64 = function(a) return %sld_f64(MEM, MEMSIZE, a) end,", x)
		b.line("    st8 = function(a, v) st8b(MEM, MEMSIZE, a, v) end,")
		b.line("    st16 = function(a, v) st16(MEM, MEMSIZE, a, v) end,")
		b.line("    st32 = function(a, v) st32(MEM, MEMSIZE, a, v) end,")
		b.line("    stf32 = function(a, v) st32(MEM, MEMSIZE, a, %sf32_to_bits(v)) end,", x)
		b.line("    stf64 = function(a, v) %sst_f64(MEM, MEMSIZE, a, v) end,", x)
		b.line("    wstr = function(a, s) fk_wstr(MEM, MEMSIZE, a, s) end,")
		b.line("    size = function() return MEMSIZE end,")
		b.line("  },")
	}
	emitPersist(b, m)
	b.line("  rt = { f32_to_bits = %sf32_to_bits, bits_to_f32 = %sbits_to_f32,", x, x)
	b.line("         f64_to_bits = %sf64_to_bits, f32 = f32, nan_mode = %q,", x, b.opts.NaN.String())
	// Box constructors let a host hand a NaN back in with its bits intact,
	// which is the only way to pass one at all in exact mode.
	b.line("         boxf32 = boxf32, boxf64 = boxf64 } }")

	src := b.sb.String()
	if err := checkChunkLocals(src, m); err != nil {
		return "", err
	}
	return src, nil
}

// maxChunkLocals is Lua's per-function local limit, which a chunk is subject to
// like anything else. Verified in third_party/lua-5.2.1/sandbox_check.lua: 200
// compile, 201 are rejected.
const maxChunkLocals = 200

// checkChunkLocals refuses a module whose chunk would blow Lua's local limit.
//
// Without this the chunk compiles here and is rejected by Lua itself -- at the
// user's game start, with "too many local variables (limit is 200)" pointing
// into generated code and naming nothing about the module that caused it.
//
// The prelude alone spends most of the budget, so what is left is a small
// number of globals. Measured today: 26 mutable i32 globals fit, 27 do not.
// Spilling the overflow into a table is the fix, and it is not free -- a global
// read becomes OP_GETUPVAL + OP_GETTABLE instead of OP_GETUPVAL, on values as
// hot as a guest's stack pointer -- so it wants a measurement rather than a
// reflex. Real TinyGo guests emit 0 or 1 global, so nothing hits this yet.
func checkChunkLocals(src string, m *ir.Module) error {
	n := countChunkLocals(src)
	if n <= maxChunkLocals {
		return nil
	}
	globals := 0
	if m.Source != nil {
		globals = len(m.Source.Globals)
	}
	return fmt.Errorf(
		"the generated chunk declares %d locals and Lua's limit is %d. "+
			"The runtime prelude takes most of the budget and this module has %d "+
			"global(s), each of which costs one local (two for an i64). "+
			"Reduce the module's globals; spilling them into a table is scheduled "+
			"but is not free, since a global read is a table index rather than an "+
			"upvalue read",
		n, maxChunkLocals, globals)
}

// countChunkLocals counts the names declared at chunk scope.
//
// Only column-zero `local` statements count: everything the prelude declares
// inside a `do ... end` block is scoped to that block and costs the chunk
// nothing, and the emitter indents those consistently.
func countChunkLocals(src string) int {
	n := 0
	for _, line := range strings.Split(src, "\n") {
		rest, ok := strings.CutPrefix(line, "local ")
		if !ok {
			continue
		}
		if strings.HasPrefix(rest, "function ") {
			n++
			continue
		}
		names, _, _ := strings.Cut(rest, "=")
		for _, name := range strings.Split(names, ",") {
			if strings.TrimSpace(name) != "" {
				n++
			}
		}
	}
	return n
}

// emitImports binds each imported function into F at its own index.
//
// Imports land in F rather than in locals of their own because F is already
// what `call` lowers to: an imported call and a local call are the same
// `F[n](...)`, so nothing downstream has to know the difference.
//
// The imports table arrives as the chunk's varargs, so a host instantiates with
// `load(src)(imports)`. Chunk varargs rather than a global because a Factorio
// mod shares its global table with nothing, but the generated chunk should not
// depend on that.
//
// **Host functions follow the same value representation as generated code**: an
// i32 is an unsigned double in [0, 2^32) (Invariant A) and an i64 is a (lo, hi)
// pair of them, passed and returned as two Lua values.
func emitImports(b *builder, m *ir.Module) {
	if m.Source == nil || len(m.Source.Imports) == 0 {
		return
	}
	b.line("-- host imports, bound from the table passed to this chunk")
	b.line("local IMPORTS = ...")
	for _, im := range m.Source.Imports {
		// The signature goes into the binding so a host that supplies the wrong
		// arity is told what was expected, at load time rather than on the
		// first call.
		b.line("F[%d] = fk_import(IMPORTS, %s, %s, %s)",
			im.Index, luaString(im.Module), luaString(im.Name),
			luaString(im.Type.String()))
	}
}

// emitModuleState declares linear memory, globals and the indirect-call table.
// All three are chunk-level locals, so generated functions capture them as
// upvalues -- one OP_GETUPVAL per access rather than a table lookup.
// branchTableID gives each large br_table a stable slot in the chunk-level BT
// table. Both the declaration pass and the emission pass derive the id the same
// way, so they cannot drift.
func branchTableID(fnIndex uint32, stepIndex int) string {
	return fmt.Sprintf("%d_%d", fnIndex, stepIndex)
}

// emitBranchTables declares every large br_table's dispatch array once, at
// chunk scope.
//
// They cannot live in the function body: Invariant B forbids a `local` after
// the prologue, and a constant table rebuilt on every call would be wasteful
// anyway. They go into one BT table rather than one local each, because Lua
// caps a chunk at 200 locals just as it does a function.
func emitBranchTables(b *builder, m *ir.Module) {
	var lines []string
	for _, f := range m.Funcs {
		for si, s := range f.Steps {
			if s.Op != wasm.OpBrTable || len(s.Targets) <= brTableChainLimit {
				continue
			}
			ids := branchTableIDs(s)
			lines = append(lines, fmt.Sprintf("BT[%q] = {%s}",
				branchTableID(f.Index, si), strings.Join(ids, ",")))
		}
	}
	if len(lines) == 0 {
		return
	}
	b.line("local BT = {}")
	for _, l := range lines {
		b.line("%s", l)
	}
}

// brTableChainLimit is the entry count below which a chain of comparisons beats
// building and indexing a table.
const brTableChainLimit = 8

// branchTableIDs maps each br_table entry to a small group id, collapsing
// duplicate destinations. Real switch statements produce tables with hundreds
// of entries and a handful of distinct targets.
func branchTableIDs(s ir.Step) []string {
	var order []ir.Branch
	idOf := func(br ir.Branch) int {
		for i, o := range order {
			if o == br {
				return i + 1
			}
		}
		order = append(order, br)
		return len(order)
	}
	ids := make([]string, len(s.Targets))
	for i, br := range s.Targets {
		ids[i] = strconv.Itoa(idOf(br))
	}
	return ids
}

// branchTableGroups returns the distinct destinations in id order.
func branchTableGroups(s ir.Step) []ir.Branch {
	var order []ir.Branch
	for _, br := range s.Targets {
		found := false
		for _, o := range order {
			if o == br {
				found = true
				break
			}
		}
		if !found {
			order = append(order, br)
		}
	}
	return order
}

func emitModuleState(b *builder, m *ir.Module) error {
	src := m.Source
	if src == nil {
		return nil
	}

	if src.Memory.Has {
		// MEM IS THE SHARD VECTOR: a 1-based array whose entry s+1 is shard s,
		// itself a 1-based table of u32 words. The 1-based choice is for #
		// semantics rather than speed: the measured penalty for 0-based is ~2%,
		// not the 3-5x that folklore claims.
		//
		// FOUR chunk-level names where there were three, and the swap that pays
		// for most of it is deliberate. MEMMAX is gone -- it was a compile-time
		// constant read at exactly one site, the memory.grow lowering, so it is
		// printed there as a numeral instead of costing every guest a chunk
		// local for the whole session. S1 is shard 0 bound directly, which is
		// exactly the status MEM itself had before; SHBOUND is
		// min(MEMSIZE, 2097152) and is what every emitted access opens on.
		// BELOW 2 MiB SHBOUND *IS* MEMSIZE, so that opening test is the bounds
		// check rather than an addition to it -- which is the entire reason the
		// below-wall cost is zero. See agents/codegen.md, "The chunk-local
		// budget", and agents/sharding.md section 5.
		//
		// The vector is built by mem_grow rather than by a loop here: shard
		// creation, the per-shard fill and the SHBOUND derivation are one piece
		// of logic, and two copies of it is how the LAST PARTIAL SHARD gets
		// built wrong. `{{}}` seeds shard 0 so that a zero-page memory still has
		// the vector shape everything else assumes.
		size := uint64(src.Memory.Min) * 65536
		bound := size
		if bound > uint64(shardBytes) {
			bound = uint64(shardBytes)
		}
		b.memMax = memMaxBytes(src.Memory)
		b.line("local MEM = {{}}")
		b.line("local MEMSIZE, SHBOUND = %d, %d", size, bound)
		b.line("mem_grow(MEM, 0, %d, %d)", b.memMax, src.Memory.Min)
		b.line("local S1 = MEM[1]")
		for _, d := range src.Data {
			if len(d.Bytes) == 0 {
				continue
			}
			b.line("do local o = %d", d.Offset)
			b.line("  local d = %s", luaString(string(d.Bytes)))
			b.line("  for i = 1, #d do st8raw(MEM, o + i - 1, string.byte(d, i)) end")
			b.line("end")
		}
	}

	for i := range src.Globals {
		// Width matters here as everywhere: an i64 global is two Lua locals.
		init := globalInit(src, i)
		b.line("local %s = %s", globalNames(src, i), init)
	}

	if src.HasTable {
		// The table holds FUNCTION INDICES, not functions, so the signature
		// check next to it stays an integer compare and element segments are
		// plain integer writes.
		b.line("local TBL, TSIG = {}, {}")
		for _, e := range src.Elems {
			for k, fi := range e.Funcs {
				b.line("TBL[%d] = %d TSIG[%d] = %d",
					int(e.Offset)+k, fi, int(e.Offset)+k, typeIndexOf(src, fi))
			}
		}
	}
	return nil
}

// emitPersist writes the surface a host uses to carry guest state across a save.
//
// MEM, MEMSIZE and the globals are chunk-level locals, captured as upvalues by
// every generated function. Lua gives one upvalue CELL per local per enclosing
// scope and every closure shares it, so assigning MEM inside `adopt` is seen by
// F[0] and by every other function in the chunk. That sharing is the entire
// mechanism -- without it a host could read guest state but never install any.
//
// MEMSIZE travels with the table because memory.grow moves it, and a guest that
// grew its heap before a save has to come back the same size or every bounds
// check after the load is computed against the wrong limit.
//
// IMMUTABLE globals are deliberately absent. They cannot change, so a reload
// rebuilds them identically from their initialisers; persisting them would
// create state that a later migration has to reconcile for no reason.
func emitPersist(b *builder, m *ir.Module) {
	src := m.Source
	if src == nil {
		return
	}
	var names []string
	for i := range src.Globals {
		if src.Globals[i].Mutable {
			names = append(names, globalNameList(src, i)...)
		}
	}
	if !src.Memory.Has && len(names) == 0 {
		return
	}

	b.line("  persist = {")
	b.line("    mode = %q,", b.opts.Persist.String())
	if src.Memory.Has && b.opts.Persist == PersistPacked {
		// Arming is what turns the page marking on. Every other mode leaves it
		// false and pays one test per store.
		b.line("    arm = MEMPACK.arm,")
		b.line("    pack = function() return MEMPACK.all(MEM, MEMSIZE) end,")
		b.line("    flush = function(t) return MEMPACK.flush(MEM, MEMSIZE, t) end,")
		// The saved size goes IN as well as onto MEMSIZE: restore walks the
		// pages that size implies rather than the array it was handed, because
		// a sparse flush leaves the array with holes in it.
		//
		// restore also rebuilds the SHARDS the saved size implies as it walks
		// the pages: a guest that grew past a shard boundary comes back to a
		// vector with fewer shards than the save has pages for. Then SHBOUND is
		// re-derived and S1 re-bound. The vector OBJECT is not replaced, so
		// storage's alias to it survives -- S1 is re-read anyway, because a
		// hoisted per-shard reference outliving a memory swap is exactly the
		// silent failure agents/sharding.md section 11 lists second, and paying
		// one upvalue write per LOAD to make it impossible is not a trade worth
		// thinking about twice.
		b.line("    restore = function(t, n) MEMPACK.restore(MEM, t, n) " +
			"MEMSIZE = n SHBOUND = n < 2097152 and n or 2097152 S1 = MEM[1] " +
			"MEMPACK.memreset(n) end,")
	}
	if src.Memory.Has && b.opts.GC == GCCollected {
		// The collector's half of the same page set. It is emitted here rather
		// than at the memio surface because it is the same mechanism packed
		// uses and because `arm` has to be reachable from control.lua's load
		// path, which is where a collection interrupted by a save gets its
		// barrier back.
		//
		// The closures capture MEM as an upvalue, which is the same sharing
		// `adopt` relies on: assigning MEM anywhere in the chunk is seen by
		// every closure in it, so a drain after a memory.grow or after a load
		// writes into the live table rather than into the one that was current
		// when this line was emitted.
		b.line("    gc = {")
		b.line("      arm = MEMPACK.gc_arm,")
		b.line("      disarm = MEMPACK.gc_disarm,")
		b.line("      drain = function(b, c) return MEMPACK.gc_drain(MEM, MEMSIZE, b, c) end,")
		b.line("    },")
	}
	b.line("    build = %s,", luaString(b.opts.BuildID))
	if src.Memory.Has {
		b.line("    memory = function() return MEM, MEMSIZE end,")
		// adopt REPLACES the vector, so every derived chunk local has to move
		// with it in the same statement: S1 is shard 0 of the NEW vector and
		// SHBOUND is min of the NEW size. A stale S1 here is the failure mode
		// with no behavioural test -- the guest reads a table nobody else can
		// reach and every answer is self-consistently wrong -- so it is pinned
		// as a text property by TestAdoptRebindsEveryDerivedMemoryLocal.
		b.line("    adopt = function(t, n) MEM = t MEMSIZE = n " +
			"SHBOUND = n < 2097152 and n or 2097152 S1 = t[1] " +
			"MEMPACK.memreset(n) end,")
		// THE PACED PRE-BUILD's two ends. `grow_hook` installs control.lua's
		// "work is owed" callback, which mem_grow calls only from the arming
		// path of a REAL grow; `prebuild` is one bounded piece of it. Both are
		// table fields rather than chunk locals for the reason DPLO/DPHI moved
		// onto MEMPACK: one more column-zero name and a guest with 32 globals
		// stops compiling at -opt=3 while still compiling at -opt=2.
		//
		// They are emitted for every persist mode and every --gc, because the
		// thing they bound -- mem_grow's zero-fill -- is paid by every guest
		// that grows, and the two growth laws that reach it (fkgc's quarter and
		// TinyGo's doubling) are both outside this file's control.
		b.line("    grow_hook = MEMPACK.grow_hook,")
		b.line("    prebuild = function(n) return MEMPACK.prebuild(MEM, n) end,")
	}
	if len(names) > 0 {
		// The destination table is optional so a caller that syncs on every
		// event can reuse one buffer instead of allocating per call. Memory
		// needs no such thing because the guest writes through to the saved
		// table; a global is a Lua local and cannot alias a table field, so
		// this is the only way it stays allocation-free.
		var get strings.Builder
		for i, n := range names {
			fmt.Fprintf(&get, "t[%d] = %s ", i+1, n)
		}
		b.line("    globals = function(t) t = t or {} %sreturn t end,", get.String())
		// One assignment per name rather than one multiple-assignment. A
		// multiple assignment needs a register per target and Lua caps a
		// function at 255, so a module with a few hundred mutable globals
		// would fail to compile -- for a saving of nothing.
		var set strings.Builder
		for i, n := range names {
			fmt.Fprintf(&set, "%s = t[%d] ", n, i+1)
		}
		set.WriteString(b.globalGuards(src))
		b.line("    setglobals = function(t) %send,", set.String())
	}
	b.line("  },")
}

// globalGuards is the check `setglobals` runs on a value it was handed.
//
// The congruence analysis proves what a global holds by induction over the
// module: its initialiser, plus every `global.set` the module itself performs.
// `setglobals` is the one door in that wall -- it writes a value from `storage`
// straight into the global -- and for a save written by the SAME build the value
// came out of this module and is inside the class by construction.
//
// A different build reached through `fk_migrate` is the exception, and it is the
// only one. Rather than argue it cannot happen, the guard makes it say so: a
// restored stack pointer that is not 8-aligned raises here, by name, instead of
// surfacing as arithmetic on a nil somewhere inside the guest, one fractional
// table index later. Nothing is emitted below -opt=3, where nothing relies on
// the class in the first place.
func (b *builder) globalGuards(src *wasm.Module) string {
	if b.ga == nil {
		return ""
	}
	var out strings.Builder
	for i := range src.Globals {
		g := src.Globals[i]
		if !g.Mutable || g.Type != wasm.I32 {
			continue
		}
		c := b.ga.At(uint32(i))
		if c.Mod <= 1 {
			continue
		}
		fmt.Fprintf(&out, "if %s %% %d ~= %d then error(\"fklua: restored global %d is not %d-aligned; the save was written by an incompatible build\", 0) end ",
			globalName(i), c.Mod, c.Res, i, c.Mod)
	}
	return out.String()
}

func memMaxBytes(m wasm.Memory) uint64 {
	if m.Max == 0 {
		return 65536 * 65536 // the wasm32 ceiling: 65536 pages
	}
	return uint64(m.Max) * 65536
}

// typeIndexOf finds a function's CANONICAL signature index -- the first type in
// the module with that structure.
//
// Canonical rather than declared, because wasm type equality is STRUCTURAL: a
// module may declare the same signature twice, and a call_indirect naming the
// second must still accept a function declared with the first. Comparing raw
// declared indices rejects those, which is what made every call in
// func_ptrs.wast trap with "indirect call type mismatch".
func typeIndexOf(m *wasm.Module, fi uint32) int {
	ft, ok := m.FuncTypeAt(fi)
	if !ok {
		return -1
	}
	return canonicalTypeIndex(m, ft)
}

// canonicalTypeIndex returns the first type index structurally equal to ft.
func canonicalTypeIndex(m *wasm.Module, ft wasm.FuncType) int {
	want := ft.String()
	for i, t := range m.Types {
		if t.String() == want {
			return i
		}
	}
	return -1
}

func globalName(i int) string { return "g" + strconv.Itoa(i) }

// globalNames names every Lua local a global occupies. An i64 global is a
// (lo, hi) pair like any other i64 value.
func globalNames(m *wasm.Module, i int) string {
	return strings.Join(globalNameList(m, i), ", ")
}

// globalNameList is globalNames as a list, for callers that have to index the
// names rather than paste them.
func globalNameList(m *wasm.Module, i int) []string {
	if m.Globals[i].Type.Slots() == 1 {
		return []string{globalName(i)}
	}
	return []string{globalName(i), globalName(i) + "h"}
}

// globalInit renders a global's initialiser, which is either a typed constant
// or a copy of an earlier global.
func globalInit(m *wasm.Module, i int) string {
	g := m.Globals[i]
	if g.InitGlobal >= 0 {
		return globalNames(m, g.InitGlobal)
	}
	switch g.Type {
	case wasm.I64:
		return u32(uint32(g.InitBits&0xFFFFFFFF)) + ", " + u32(uint32(g.InitBits>>32))
	case wasm.F32:
		return f32Literal(uint32(g.InitBits))
	case wasm.F64:
		return f64Literal(g.InitBits)
	}
	return u32(uint32(g.InitBits))
}

func labelName(l ir.Label) string { return "L" + strconv.Itoa(int(l)) }

// forCtrlName is the counted loop's own control variable, emitted only for the
// multi-exit shape that cannot borrow the wasm local. Indexed by a STEP index,
// like the loop guard's two names -- which is exactly the reason the guard's
// prefix had to move; see the namespace note on guardName.
func forCtrlName(header int) string { return "fk" + strconv.Itoa(header) }

// upvalName is a hot callee promoted to a chunk-level upvalue at -opt=3,
// indexed by FUNCTION index.
func upvalName(fi uint32) string { return "fu" + strconv.FormatUint(uint64(fi), 10) }

func emitFunc(b *builder, f *ir.Func) error {
	var params []string
	for i, pt := range f.Params {
		base := f.LocalSlot(uint32(i))
		for k := 0; k < pt.Slots(); k++ {
			params = append(params, b.slotName(base+ir.Slot(k)))
		}
	}

	b.w, b.fr, b.al = nil, nil, nil
	b.sp = b.plans[f.Index]
	if b.opt.Peephole() && f.Unsupported == nil {
		b.w = analysis.Ranges(f)
	}
	if b.opt.Slots() && f.Unsupported == nil {
		b.fr = analysis.Frames(f)
	}
	// The congruence analysis is asked for only where something consumes it:
	// the inlined i32 load, which exists at -opt=3 and nowhere else. Gating it
	// on the same predicate is what keeps -opt=0, 1 and 2 byte-identical by
	// construction rather than by care.
	if b.inlineLoads() && f.Unsupported == nil {
		b.al = analysis.Aligns(f, b.w, b.ga)
	}
	b.planCountedLoops(f)
	b.planLoopGuards(f)

	b.line("-- %s %s", f.Name, signature(f))

	// A function FkLua cannot compile still exists and is still callable; it
	// raises a distinguishable "unsupported" error rather than a wasm trap, so
	// an unimplemented feature can never be mistaken for a working one.
	if f.Unsupported != nil {
		b.line("F[%d] = function() unsupported(%q) end", f.Index, f.Unsupported.Error())
		return nil
	}

	// A guest's own memcpy/memset is a byte loop the runtime already does far
	// better, so its body is replaced wholesale rather than compiled. Worth
	// 3.97x on a copy-heavy guest and nothing at all on one that copies a few
	// bytes at a time -- see analysis.NativeIntrinsic for both numbers and for
	// exactly what the substitution changes.
	if b.nativeIntrinsics() {
		switch analysis.NativeIntrinsic(f) {
		case analysis.IntrinsicCopy:
			b.line("-- body replaced by the runtime's own mem_copy")
			b.line("F[%d] = function(v0, v1, v2) mem_copy(MEM, MEMSIZE, v0, v1, v2) return v0 end",
				f.Index)
			return nil
		case analysis.IntrinsicFill:
			b.line("-- body replaced by the runtime's own mem_fill")
			b.line("F[%d] = function(v0, v1, v2) mem_fill(MEM, MEMSIZE, v0, v1, v2) return v0 end",
				f.Index)
			return nil
		}
	}

	b.line("F[%d] = function(%s)", f.Index, strings.Join(params, ", "))
	b.indent++

	// Prologue. Everything the body will touch is declared here and nowhere
	// else, which is what makes goto-based control flow legal later (Invariant
	// B). Declared wasm locals are zero-initialised because the spec requires
	// it and Lua would otherwise leave them nil.
	// Declared locals are zero-initialised because the spec requires it and Lua
	// would otherwise leave them nil. Widths matter here: an i64 local needs two
	// names and two zeros.
	// The frame base has to be a local, and it has to be the FIRST one: every
	// spilled slot below is named relative to it.
	if b.sp.Active() {
		b.line("-- %d slot(s) spilled to the chunk-level frame stack", b.sp.Size)
		b.line("local fb = FP FP = FP + %d", b.sp.Size)
	}
	if len(f.Locals) > 0 {
		var names, zeros, spills []string
		for i, lt := range f.Locals {
			base := f.LocalSlot(uint32(len(f.Params) + i))
			for k := 0; k < lt.Slots(); k++ {
				slot := base + ir.Slot(k)
				if _, ok := b.sp.At(slot); ok {
					// A spilled wasm local still has to start at zero, and a
					// frame-stack entry holds whatever the last call left.
					spills = append(spills, b.slotName(slot)+" = 0")
					continue
				}
				names = append(names, b.slotName(slot))
				zeros = append(zeros, "0")
			}
		}
		if len(names) > 0 {
			b.line("local %s = %s", strings.Join(names, ", "), strings.Join(zeros, ", "))
		}
		for _, l := range spills {
			b.line("%s", l)
		}
	}
	if f.MaxStack > 0 {
		var names []string
		base := f.ResultSlot()
		for i := 0; i < f.MaxStack; i++ {
			slot := base + ir.Slot(i)
			if _, ok := b.sp.At(slot); ok {
				// An operand-stack slot is written before it is read, so a
				// spilled one needs no initialiser -- exactly as an undeclared
				// Lua local needs none.
				continue
			}
			names = append(names, b.slotName(slot))
		}
		if len(names) > 0 {
			b.line("local %s", strings.Join(names, ", "))
		}
	}
	// Typed-slot promotion. A shadow-stack slot whose address never leaves the
	// function becomes a Lua local, so the store and the matching load both
	// vanish -- and for an f64 that is an IEEE-754 disassembly and reassembly
	// per access, not just a table write.
	//
	// Zero-initialised, because the frame's memory was not: it holds whatever
	// the last callee left there. wasm would read those bytes; a promoted local
	// would read nil and raise a Lua error a long way from the cause.
	if b.fr.Promoted() {
		var names, zeros []string
		var where []string
		for _, fs := range b.fr.Slots {
			for k := 0; k < fs.Type.Slots(); k++ {
				names = append(names, b.slotName(fs.Base+ir.Slot(k)))
				zeros = append(zeros, "0")
			}
			where = append(where, fmt.Sprintf("%s@%d:%s",
				b.slotName(fs.Base), fs.Offset, fs.Type))
		}
		b.line("-- promoted shadow-stack slots: %s", strings.Join(where, " "))
		b.line("local %s = %s", strings.Join(names, ", "), strings.Join(zeros, ", "))
	}

	// Scratch registers for lowerings that need a temporary. Declared in the
	// prologue like everything else; unused ones cost nothing at runtime.
	//
	// The inlined accesses need them too -- the load, the 8-byte accesses, and
	// the inlined i32 store. Asking about it here rather than inside
	// needsScratch keeps the level out of that function: adding OpI32Load or
	// OpI32Store unconditionally would declare t0 in every function with a
	// memory access at EVERY level, and -opt=0 has to keep reproducing the M4
	// emitter byte for byte.
	//
	// Getting this wrong is not a compile error. `t0 = ...` with no `local t0`
	// in scope is a write to a GLOBAL: it parses, it runs, it computes the
	// right answer, and it turns every scratch access into an _ENV lookup --
	// which is how the inlined load nearly shipped a 1.28x SLOWDOWN past a
	// green spec suite. TestAFunctionUsingAScratchRegisterDeclaresIt is the
	// gate; extend it when a new lowering reaches for t0.
	switch b.scratchCount(f) {
	case 2:
		b.line("local t0, t1")
	case 4:
		b.line("local t0, t1, t2, t3")
	}
	// One flag per guarded loop. Declared here for the same reason everything
	// else is: Invariant B admits no `local` after the first ::label::, and a
	// guard is written at a loop header, which is one.
	if gl := b.guardLocals(); len(gl) > 0 {
		b.line("local %s", strings.Join(gl, ", "))
	}

	fw := forward(b, f)
	for i := range f.Steps {
		if fw.elided[i] {
			continue
		}
		// The guard belongs to the loop HEADER, not to the shape that header
		// is emitted as, so it is written here -- once, in front of whichever
		// form follows -- rather than inside the OpLoop case below.
		//
		// It used to live there, and that was the bug: a counted loop replaces
		// its header with a `for` and never reaches OpLoop, so the seed was the
		// one part of the guard that went missing while the declaration, the
		// guarded arms and the word-index stepping were all still emitted. The
		// flag then stayed false for the life of the call -- every access took
		// the checked arm, every answer was right, and the entire win was gone
		// with nothing behavioural able to see it.
		if g, ok := b.lg[i]; ok {
			b.emitLoopGuard(f, g)
		}
		// A counted loop replaces its own header, its increment and its test.
		// The body in between is emitted exactly as it would have been, which
		// is what keeps this a control-flow change and nothing more.
		if c, ok := b.cl[i]; ok {
			b.emitForHeader(f, c, fw)
			continue
		}
		if c, ok := b.clEnd[i]; ok {
			b.emitForEnd(c, fw)
			continue
		}
		if b.clDrop[i] {
			continue
		}
		if err := emitStep(b, f, i, fw); err != nil {
			return err
		}
		// Every word index whose base advances here moves with it.
		b.emitWordSteps(i)
	}

	// Fall-through return. wasm leaves results on the stack at `end`; the
	// slots holding them are the topmost ones -- unless the peephole handed
	// over the expression that would have been written into one.
	if len(f.Results) > 0 {
		if fw.retExpr != "" {
			b.line("%sreturn %s", b.unwind(), fw.retExpr)
		} else {
			base := f.ResultSlot()
			var names []string
			for _, rt := range f.Results {
				names = append(names, b.slotNames(base, rt))
				base += ir.Slot(rt.Slots())
			}
			b.line("%sreturn %s", b.unwind(), strings.Join(names, ", "))
		}
	} else if b.sp.Active() {
		// Nothing to return, but the frame still has to be given back.
		b.line("FP = fb")
	}

	b.indent--
	b.line("end")
	return nil
}

// needsScratch reports whether any lowering in this function needs a temporary.
// Scratch registers are declared in the prologue like everything else; an
// unused pair costs nothing at runtime.
// hasOp reports whether the function contains op at all.
func hasOp(f *ir.Func, op wasm.Op) bool {
	for _, s := range f.Steps {
		if s.Op == op {
			return true
		}
	}
	return false
}

// scratchCount is how many scratch registers this function's prologue must
// declare: 0, 2 or 4.
//
// A scratch is a PRE-DECLARED local, never a bare assignment. `t0 = ...` in a
// function whose prologue never declared it is a write to a GLOBAL -- it
// parses, it runs, it computes the right answer, and it turns every scratch
// access into an _ENV lookup while scribbling a name into the mod's global
// table. That already shipped once with the inlined i32 load;
// TestAFunctionUsingAScratchRegisterDeclaresIt is what watches for it, and
// every count below has to stay in step with the lowerings that reach for one.
//
// Four rather than two only for the inlined 8-byte f64 access, which has to
// hold the address, the two halves of the bit pattern and the exponent at once.
func (b *builder) scratchCount(f *ir.Func) int {
	n := 0
	if needsScratch(f) {
		n = 2
	}
	need := func(k int) {
		if k > n {
			n = k
		}
	}
	if b.inlineLoads() {
		if hasOp(f, wasm.OpI32Load) || hasOp(f, wasm.OpI64Load) {
			need(2)
		}
		if hasOp(f, wasm.OpF64Load) {
			need(4)
		}
	}
	if b.inlineStores() && hasOp(f, wasm.OpI32Store) {
		need(2)
	}
	// The inlined byte loads. An 8-bit one holds the address, the byte's
	// position-derived divisor and the containing word -- three, so the pair is
	// not enough. A 16-bit one also holds the low byte while it fetches the
	// high one, which is the fourth.
	if b.inlineByteLoads() {
		if hasOp(f, wasm.OpI32Load8U) || hasOp(f, wasm.OpI32Load8S) {
			need(4)
		}
		if hasOp(f, wasm.OpI32Load16U) || hasOp(f, wasm.OpI32Load16S) {
			need(4)
		}
	}
	if b.inlineWideStores() {
		if hasOp(f, wasm.OpI64Store) {
			need(2)
		}
		if hasOp(f, wasm.OpF64Store) {
			need(4)
		}
	}
	return n
}

func needsScratch(f *ir.Func) bool {
	for _, s := range f.Steps {
		switch s.Op {
		case wasm.OpI32Shl, wasm.OpI32ShrU, wasm.OpI32ShrS,
			wasm.OpI32Rotl, wasm.OpI32Rotr,
			wasm.OpI32Extend8S, wasm.OpI32Extend16S,
			wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS,
			wasm.OpCallIndirect, wasm.OpBrTable,
			wasm.OpI32Load8S, wasm.OpI32Load16S,
			wasm.OpI64Extend8S, wasm.OpI64Extend16S, wasm.OpI64Extend32S,
			wasm.OpI64Load8S, wasm.OpI64Load16S, wasm.OpI64Load32S:
			return true
		}
	}
	return false
}

// isControlFlow reports ops that end a basic block, either by jumping or by
// defining a label something else can jump to.
func isControlFlow(op wasm.Op) bool {
	switch op {
	case wasm.OpBlock, wasm.OpLoop, wasm.OpIf, wasm.OpElse, wasm.OpEnd,
		wasm.OpBr, wasm.OpBrIf, wasm.OpBrTable, wasm.OpReturn, wasm.OpUnreachable:
		return true
	}
	return false
}

// constArg reports an operand's value when it came from an i32.const that was
// forwarded into this instruction. This is what drives constant specialisation
// of multiply, shifts and masks.
func constArg(fw *forwarding, stepIdx, argIdx int) (uint32, bool) {
	if argIdx >= len(fw.konst[stepIdx]) {
		return 0, false
	}
	if v := fw.konst[stepIdx][argIdx]; v != nil {
		return *v, true
	}
	return 0, false
}

// argExpr renders operand k of step i, honouring both operand forwarding and
// operand WIDTH.
//
// Two facts have to hold at once. A forwarded operand's producing step was
// elided, so its slot was never written and reading b.slotName(s.Args[k]) yields
// nil -- the expression must come from the forwarding table. And a wide operand
// occupies two Lua locals, so naming only its base drops the high half. The
// two never collide: forwarding is barred from wide values, so a wide operand
// is always a plain slot pair.
func (b *builder) argExpr(s ir.Step, fw *forwarding, i, k int) string {
	if k < len(s.ArgTypes) && s.ArgTypes[k].Slots() > 1 {
		return b.slotNames(s.Args[k], s.ArgTypes[k])
	}
	return fw.raw[i][k]
}

func emitStep(b *builder, f *ir.Func, i int, fw *forwarding) error {
	s := f.Steps[i]
	d := ""
	if s.Dst != ir.NoSlot {
		d = b.slotName(s.Dst)
	}
	// Operands are expressions, not necessarily slot names: a forwarded
	// local.get, i32.const, or -- from -opt=1 -- any single-expression step
	// appears inline here.
	//
	// a and c are the NARROW forms, correct for every single-slot operand and
	// for the address/condition operands of the wide ops. An operand that is
	// itself wide must go through argExpr, which names both of its halves.
	var a, c string
	if len(fw.args[i]) > 0 {
		a = fw.args[i][0]
	}
	if len(fw.args[i]) > 1 {
		c = fw.args[i][1]
	}
	// assign skips a self-assignment such as `v1 = v1`, which the identity
	// lowerings (shift by zero, and with all ones) would otherwise emit.
	assign := func(dst, src string) {
		if dst != src {
			b.line("%s = %s", dst, src)
		}
	}

	// Typed-slot promotion turns a frame store into a plain assignment and a
	// frame load into a plain read. Both are checked before anything else,
	// because the op itself still says "i64.store" and every lowering below
	// would happily emit the memory access it no longer needs.
	if fs, ok := b.fr.StoreAt(i); ok {
		src := c
		if fs.Type.Slots() > 1 {
			src = b.slotNames(s.Args[1], s.ArgTypes[1])
		}
		b.line("%s = %s", b.slotNames(fs.Base, fs.Type), src)
		return nil
	}
	if fs, ok := b.fr.LoadAt(i); ok && fs.Type.Slots() > 1 {
		b.line("%s = %s", b.slotNames(s.Dst, s.DstType), b.slotNames(fs.Base, fs.Type))
		return nil
	}

	// Every op that lowers to one assignment lives in stepExpr, so the text a
	// step emits and the text the peephole substitutes into its consumer are
	// produced by the same code.
	if e, ok := stepExpr(b, f, i, fw); ok {
		assign(d, e.text)
		return nil
	}

	// cond renders the branch condition for this step, folding in a comparison
	// the peephole handed over. `negate` asks for the inverted test, which is
	// what `if` needs: it jumps to the else-label when the condition is FALSE.
	cond := func(operand string, negate bool) string {
		if j := fw.condFrom[i]; j >= 0 {
			if text, ok := condExpr(b, f, j, fw, negate); ok {
				return text
			}
		}
		if negate {
			return operand + " == 0"
		}
		return operand + " ~= 0"
	}

	// emitBranch writes an edge: the value copy it carries, then the jump.
	// A branch out of the function is a return, which Lua requires to be the
	// last statement in a block -- hence `do return end`, verified against the
	// interpreter (a bare mid-block `return` is a syntax error).
	emitBranch := func(br ir.Branch) {
		if br.From != ir.NoSlot && br.To != ir.NoSlot && br.From != br.To {
			b.line("%s = %s", b.slotNames(br.To, br.Typ), b.slotNames(br.From, br.Typ))
		}
		if br.IsReturn() {
			if len(f.Results) > 0 {
				b.line("do %sreturn %s end", b.unwind(),
					b.slotNames(f.ResultSlot(), f.Results[0]))
			} else {
				b.line("do %sreturn end", b.unwind())
			}
			return
		}
		b.line("goto %s", labelName(br.Label))
	}

	switch s.Op {
	case wasm.OpNop:
		return nil

	// -- control flow ------------------------------------------------------
	//
	// Everything is emitted flat, at function-body level. Lua rejects a goto
	// into a sibling block, so nesting the constructs would make most labels
	// invisible; flat emission also keeps the parser clear of its
	// LUAI_MAXCCALLS=200 nesting limit, which asyncify-transformed guests hit.
	case wasm.OpBlock:
		return nil // a block contributes only its end label

	case wasm.OpLoop:
		// The loop guard is NOT written here. It is written by the caller,
		// before this step is dispatched at all, because a counted loop
		// replaces this step and would otherwise take the guard's seed with it.
		b.line("::%s::", labelName(s.Label))
		// Charged at the loop HEADER rather than at each back edge: every
		// iteration passes through here exactly once, and a loop with four
		// `continue`s would otherwise need four copies of this. Entering the
		// loop also charges one, which is off by one per loop and not worth a
		// branch to avoid.
		if b.opts.Fuel > 0 {
			b.line("FUEL = FUEL - 1 if FUEL < 0 then trap_fuel() end")
		}
		return nil

	case wasm.OpIf:
		b.line("if %s then goto %s end", cond(a, true), labelName(s.ElseLabel))
		return nil

	case wasm.OpElse:
		// Falling out of the then-arm must skip the else-arm.
		b.line("goto %s", labelName(s.Label))
		b.line("::%s::", labelName(s.ElseLabel))
		return nil

	case wasm.OpEnd:
		if s.Label == ir.NoLabel {
			return nil // function-level end
		}
		// An `if` with no else still needs its else label, or the false branch
		// has nowhere to land.
		if s.ElseLabel != ir.NoLabel {
			b.line("::%s::", labelName(s.ElseLabel))
		}
		b.line("::%s::", labelName(s.Label))
		return nil

	case wasm.OpBr:
		emitBranch(s.Target)
		return nil

	case wasm.OpBrIf:
		if s.Target.From == ir.NoSlot && !s.Target.IsReturn() {
			// The common case collapses to one line.
			b.line("if %s then goto %s end", cond(a, false), labelName(s.Target.Label))
			return nil
		}
		b.line("if %s then", cond(a, false))
		b.indent++
		emitBranch(s.Target)
		b.indent--
		b.line("end")
		return nil

	case wasm.OpBrTable:
		return emitBrTable(b, f, i, s, a, emitBranch)

	case wasm.OpUnreachable:
		b.line("trap_unreachable()")
		return nil

	case wasm.OpReturn:
		if len(s.Args) > 0 {
			// An i64 result leaves the function as two Lua values, so the
			// returned expression has to name both halves.
			b.line("do %sreturn %s end", b.unwind(), b.argExpr(s, fw, i, 0))
		} else {
			b.line("do %sreturn end", b.unwind())
		}
		return nil

	case wasm.OpSelect:
		// select returns its first operand when the condition is non-zero.
		// `and/or` would mis-handle a false-y first operand, and in exact mode
		// an operand may be a table, so this is written out longhand.
		// The operands must come from the forwarding table, not from raw slot
		// names: a forwarded operand's producing step was elided, so its slot
		// was never written and reading it yields nil.
		st := s.DstType
		lhs, rhs := fw.args[i][0], fw.args[i][1]
		if st.Slots() > 1 {
			lhs, rhs = b.slotNames(s.Args[0], st), b.slotNames(s.Args[1], st)
		}
		b.line("if %s then %s = %s else %s = %s end", cond(fw.args[i][2], false),
			b.slotNames(s.Dst, st), lhs, b.slotNames(s.Dst, st), rhs)
		return nil

	// -- calls and globals -------------------------------------------------
	case wasm.OpCall:
		args := b.callArgs(s, fw.raw[i])
		if s.Results > 0 {
			// Lua returns multiple values natively, so an N-result call needs no
			// packing. This is also the mechanism i64 uses, where a single wasm
			// value becomes a (lo, hi) pair.
			b.line("%s = %s(%s)", b.resultList(s), b.callName(s.Callee), strings.Join(args, ", "))
		} else {
			b.line("%s(%s)", b.callName(s.Callee), strings.Join(args, ", "))
		}
		return nil

	case wasm.OpCallIndirect:
		// The table index is the last operand, above the call arguments.
		n := len(s.Args)
		idx := fw.args[i][n-1]
		args := b.callArgs(ir.Step{Args: s.Args[:n-1], ArgTypes: s.ArgTypes[:n-1]},
			fw.raw[i][:n-1])
		b.line("t0 = TBL[%s]", idx)
		b.line("if t0 == nil then trap_uninit() end")
		// Compare canonical indices on both sides, so two structurally identical
		// type declarations are interchangeable, as the spec requires.
		want := int(s.CallType)
		if f.Mod != nil && int(s.CallType) < len(f.Mod.Types) {
			want = canonicalTypeIndex(f.Mod, f.Mod.Types[s.CallType])
		}
		b.line("if TSIG[%s] ~= %d then trap_indirect() end", idx, want)
		if s.Results > 0 {
			b.line("%s = F[t0](%s)", b.resultList(s), strings.Join(args, ", "))
		} else {
			b.line("F[t0](%s)", strings.Join(args, ", "))
		}
		return nil

	case wasm.OpGlobalGet:
		// Only the wide case reaches here; stepExpr handles the rest.
		gi := int(s.Instr.GlobalIndex)
		b.line("%s = %s", b.slotNames(s.Dst, s.DstType), globalNames(f.Mod, gi))
		return nil

	case wasm.OpGlobalSet:
		gi := int(s.Instr.GlobalIndex)
		if s.ArgTypes[0].Slots() > 1 {
			b.line("%s = %s", globalNames(f.Mod, gi),
				b.slotNames(s.Args[0], s.ArgTypes[0]))
			return nil
		}
		b.line("%s = %s", globalName(gi), a)
		return nil

	// -- linear memory -----------------------------------------------------
	case wasm.OpI32Load8U:
		if b.inlineByteLoads() {
			b.emitInlineLoad8(a, s.Instr.MemOffset, d, false)
			return nil
		}
		b.line("%s = ld8(MEM, MEMSIZE, %s)", d, addrExpr(a, s.Instr.MemOffset))
		return nil
	case wasm.OpI32Load16U:
		if b.inlineByteLoads() {
			b.emitInlineLoad16(a, s.Instr.MemOffset, d, false)
			return nil
		}
		b.line("%s = ld16(MEM, MEMSIZE, %s)", d, addrExpr(a, s.Instr.MemOffset))
		return nil
	case wasm.OpI32Load8S:
		if b.inlineByteLoads() {
			b.emitInlineLoad8(a, s.Instr.MemOffset, d, true)
			return nil
		}
		b.line("t0 = ld8(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset))
		b.line("%s = t0 >= 128 and t0 + 4294967040.0 or t0", d)
		return nil
	case wasm.OpI32Load16S:
		if b.inlineByteLoads() {
			b.emitInlineLoad16(a, s.Instr.MemOffset, d, true)
			return nil
		}
		b.line("t0 = ld16(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset))
		b.line("%s = t0 >= 32768 and t0 + 4294901760.0 or t0", d)
		return nil
	case wasm.OpI32Store:
		if b.inlineStores() {
			b.emitInlineStore32(s, fw, i, a, c)
			return nil
		}
		b.line("st32(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), c)
		return nil
	// store8 and store16 stay CALLS at every level, and that is a measured-shape
	// judgement rather than an oversight. st32's aligned fast path is one table
	// write, so inlining it replaces a call with three lines. st8b's body is a
	// read-modify-write of the containing word -- it needs the byte's position,
	// the word index, a power-of-two divisor from P2 and the old word all live
	// at once, which is five values against the two scratch registers a function
	// declares. Expanding it would have to widen the scratch file, so it is its
	// own experiment with its own measurement, not a rider on this one.
	case wasm.OpI32Store8:
		b.line("st8b(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), c)
		return nil
	case wasm.OpI32Store16:
		b.line("st16(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), c)
		return nil
	case wasm.OpMemoryGrow:
		// SHBOUND moves with MEMSIZE and comes back from the same call, so a
		// grow stays ONE statement: deriving min(MEMSIZE, 2097152) at the call
		// site would be a second comparison emitted at every memory.grow, and
		// forgetting it would leave the fast path bounded by the OLD size --
		// correct, silently slower, and invisible to every checksum.
		//
		// The maximum is printed as a numeral rather than read from a chunk
		// local: it is a compile-time constant with exactly this one reader, and
		// the local it used to occupy is what S1 and SHBOUND are spending. See
		// agents/codegen.md, "The chunk-local budget".
		b.line("%s, MEMSIZE, SHBOUND = mem_grow(MEM, MEMSIZE, %d, %s)",
			d, b.memMax, a)
		return nil
	case wasm.OpMemoryCopy, wasm.OpMemoryFill:
		// Three operands, so the third comes from the forwarding table rather
		// than from a or c. Both are runtime helpers rather than inline loops:
		// the whole point is one bounds check and one dirty-page mark
		// for the range instead of one per byte.
		fn := "mem_copy"
		if s.Instr.Op == wasm.OpMemoryFill {
			fn = "mem_fill"
		}
		b.line("%s(MEM, MEMSIZE, %s, %s, %s)", fn,
			fw.args[i][0], fw.args[i][1], fw.args[i][2])
		return nil

	case wasm.OpF32Store:
		if b.exact() {
			b.line("xst_f32(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), c)
			return nil
		}
		b.line("st32(MEM, MEMSIZE, %s, f32_to_bits(%s))", addrExpr(a, s.Instr.MemOffset), c)
		return nil
	case wasm.OpF64Store:
		// st_f64 is three calls deep on the aligned path -- st_f64, f64_to_bits
		// and st64. Inlining removes two of the three; f64_to_bits stays,
		// because expanding an IEEE-754 disassembly with all its special cases
		// at every store site would be a page of Lua for no call saved.
		//
		// The VALUE goes into a scratch BEFORE the bounds check, which is not
		// cosmetic. wasm evaluates address, then value, then performs the
		// access, so a trapping value expression must trap before the store's
		// own out-of-bounds check does -- and the two traps carry different
		// codes, which the conformance suite compares.
		if b.inlineWideStores() {
			// Same straddle argument as the i64 store: the merged test proves
			// both words in shard 0, and everything else is st_f64's.
			b.line("t0 = %s", addrExpr(a, s.Instr.MemOffset))
			b.line("t1 = %s", c)
			b.line("if %s then t2, t3 = %sf64_to_bits(t1) t1 = t0 / 4 + 1 S1[t1] = t2 S1[t1 + 1] = t3",
				shardFast(8, false), b.pfx())
			b.line("else %sst_f64(MEM, MEMSIZE, t0, t1) end", b.pfx())
			return nil
		}
		b.line("%sst_f64(MEM, MEMSIZE, %s, %s)", b.pfx(), addrExpr(a, s.Instr.MemOffset), c)
		return nil

	case wasm.OpDrop:
		// The value stays in its slot and is simply never read again.
		return nil

	// A local is as wide as its declared type: an i64 local is a (lo, hi) pair,
	// so all three of these move TWO Lua locals. Naming only the base would
	// leave the high half unwritten, and the nil surfaces far away -- inside an
	// i64 helper, or as a return that hands back one value where the caller
	// unpacks two.
	case wasm.OpLocalGet:
		lt := f.LocalType(s.Instr.LocalIndex)
		b.line("%s = %s", b.slotNames(s.Dst, lt), b.slotNames(f.LocalSlot(s.Instr.LocalIndex), lt))

	case wasm.OpLocalSet:
		lt := f.LocalType(s.Instr.LocalIndex)
		b.line("%s = %s", b.slotNames(f.LocalSlot(s.Instr.LocalIndex), lt), b.argExpr(s, fw, i, 0))

	case wasm.OpLocalTee:
		lt := f.LocalType(s.Instr.LocalIndex)
		src := b.argExpr(s, fw, i, 0)
		b.line("%s = %s", b.slotNames(f.LocalSlot(s.Instr.LocalIndex), lt), src)
		// tee pops and pushes the same type, so its result lands back in the
		// operand's own slots and the copy is almost always a self-assignment.
		assign(b.slotNames(s.Dst, s.DstType), src)

	// -- shifts and rotates whose constant form needs a scratch register ------
	case wasm.OpI32ShrS:
		k, _ := b.constOf(fw, i, 1)
		n := k % 32
		if n == 0 {
			assign(d, a)
			return nil
		}
		b.line("t0 = %s if t0 >= %s then t0 = t0 - %s end", a, signMin, wrapMod)
		b.line("%s = ((t0 - t0 %% %s) / %s) %% %s", d, u32(1<<n), u32(1<<n), wrapMod)
		return nil

	case wasm.OpI32Rotl:
		k, _ := b.constOf(fw, i, 1)
		n := k % 32
		if n == 0 {
			assign(d, a)
			return nil
		}
		// The two halves occupy disjoint bit ranges, so + is exact and bor is
		// unnecessary.
		lo := uint32(1) << (32 - n)
		b.line("t0 = %s %% %s", a, u32(lo))
		b.line("%s = t0 * %s + (%s - t0) / %s", d, u32(1<<n), a, u32(lo))
		return nil

	case wasm.OpI32Rotr:
		k, _ := b.constOf(fw, i, 1)
		n := k % 32
		if n == 0 {
			assign(d, a)
			return nil
		}
		b.line("t0 = %s %% %s", a, u32(1<<n))
		b.line("%s = t0 * %s + (%s - t0) / %s", d, u32(1<<(32-n)), a, u32(1<<n))
		return nil

	// -- unary needing a scratch register ------------------------------------
	case wasm.OpI32Extend8S:
		b.line("t0 = %s %% 256", a)
		b.line("%s = t0 >= 128 and t0 + 4294967040.0 or t0", d)

	case wasm.OpI32Extend16S:
		b.line("t0 = %s %% 65536", a)
		b.line("%s = t0 >= 32768 and t0 + 4294901760.0 or t0", d)

	// -- signed comparison, -opt=0 form --------------------------------------
	//
	// Two conditional sign fixups through scratch registers. From -opt=1 this
	// becomes a single biased expression in condExpr, which is branch-free and
	// can be folded straight into the branch that consumes it.
	case wasm.OpI32LtS, wasm.OpI32LeS, wasm.OpI32GtS, wasm.OpI32GeS:
		op := map[wasm.Op]string{
			wasm.OpI32LtS: "<", wasm.OpI32LeS: "<=",
			wasm.OpI32GtS: ">", wasm.OpI32GeS: ">=",
		}[s.Op]
		b.line("t0 = %s if t0 >= %s then t0 = t0 - %s end", a, signMin, wrapMod)
		b.line("t1 = %s if t1 >= %s then t1 = t1 - %s end", c, signMin, wrapMod)
		b.line("%s = t0 %s t1 and 1 or 0", d, op)

	// -- i64 ----------------------------------------------------------------
	//
	// An i64 is a (lo, hi) pair of unsigned doubles, never a boxed table:
	// boxing would cost an allocation and a metamethod dispatch per operation,
	// which is GC pressure inside a lockstep game loop. Helpers take and return
	// halves through Lua's native multiple return, so nothing needs packing.
	case wasm.OpI64Const:
		b.line("%s = %s, %s", b.slotNames(s.Dst, wasm.I64),
			u32(uint32(s.Instr.I64&0xFFFFFFFF)), u32(uint32(s.Instr.I64>>32)))
		return nil
	case wasm.OpI64Add:
		b.line("%s = i64_add(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64Sub:
		b.line("%s = i64_sub(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64Mul:
		b.line("%s = i64_mul(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64DivS:
		b.line("%s = i64_divs(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64DivU:
		b.line("%s = i64_divu(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64RemS:
		b.line("%s = i64_rems(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64RemU:
		b.line("%s = i64_remu(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64Shl:
		b.line("%s = i64_shl(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64ShrS:
		b.line("%s = i64_shrs(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64ShrU:
		b.line("%s = i64_shru(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64Rotl:
		b.line("%s = i64_rotl(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64Rotr:
		b.line("%s = i64_rotr(%s, %s)", b.slotNames(s.Dst, wasm.I64),
			b.slotNames(s.Args[0], wasm.I64), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64And:
		// Two independent i32 operations; the halves never interact.
		b.line("%s = band(%s, %s)", b.slotName(s.Dst), b.slotName(s.Args[0]), b.slotName(s.Args[1]))
		b.line("%s = band(%s, %s)", b.slotName(s.Dst+1), b.slotName(s.Args[0]+1), b.slotName(s.Args[1]+1))
		return nil
	case wasm.OpI64Or:
		b.line("%s = bor(%s, %s)", b.slotName(s.Dst), b.slotName(s.Args[0]), b.slotName(s.Args[1]))
		b.line("%s = bor(%s, %s)", b.slotName(s.Dst+1), b.slotName(s.Args[0]+1), b.slotName(s.Args[1]+1))
		return nil
	case wasm.OpI64Xor:
		b.line("%s = bxor(%s, %s)", b.slotName(s.Dst), b.slotName(s.Args[0]), b.slotName(s.Args[1]))
		b.line("%s = bxor(%s, %s)", b.slotName(s.Dst+1), b.slotName(s.Args[0]+1), b.slotName(s.Args[1]+1))
		return nil
	// clz/ctz/popcnt return an i64, not an i32 -- the count is at most 64, so
	// the high half is always zero, but it still has to be WRITTEN. Leaving it
	// alone would hand the next consumer a nil or, worse, a stale half from
	// whatever last occupied the slot.
	case wasm.OpI64Clz:
		b.line("%s = i64_clz(%s), 0", b.slotNames(s.Dst, wasm.I64), b.slotNames(s.Args[0], wasm.I64))
		return nil
	case wasm.OpI64Ctz:
		b.line("%s = i64_ctz(%s), 0", b.slotNames(s.Dst, wasm.I64), b.slotNames(s.Args[0], wasm.I64))
		return nil
	case wasm.OpI64Popcnt:
		b.line("%s = i64_popcnt(%s), 0", b.slotNames(s.Dst, wasm.I64), b.slotNames(s.Args[0], wasm.I64))
		return nil

	case wasm.OpI64Extend8S, wasm.OpI64Extend16S, wasm.OpI64Extend32S:
		// Narrow the low half, then splash the sign across the high half.
		switch s.Op {
		case wasm.OpI64Extend8S:
			b.line("t0 = %s %% 256", b.slotName(s.Args[0]))
			b.line("if t0 >= 128 then t0 = t0 + 4294967040.0 end")
		case wasm.OpI64Extend16S:
			b.line("t0 = %s %% 65536", b.slotName(s.Args[0]))
			b.line("if t0 >= 32768 then t0 = t0 + 4294901760.0 end")
		default:
			b.line("t0 = %s", b.slotName(s.Args[0]))
		}
		b.line("%s = t0, t0 >= 2147483648.0 and 4294967295.0 or 0",
			b.slotNames(s.Dst, wasm.I64))
		return nil

	// -- i64 conversions ----------------------------------------------------
	case wasm.OpI64ExtendI32U:
		b.line("%s = %s, 0", b.slotNames(s.Dst, wasm.I64), a)
		return nil
	case wasm.OpI64ExtendI32S:
		b.line("%s = %s, %s >= 2147483648.0 and 4294967295.0 or 0",
			b.slotNames(s.Dst, wasm.I64), a, a)
		return nil
	case wasm.OpI64TruncSatF32S, wasm.OpI64TruncSatF64S:
		b.line("%s = %si64_trunc_sat_s(%s)", b.slotNames(s.Dst, wasm.I64), b.pfx(), a)
		return nil
	case wasm.OpI64TruncSatF32U, wasm.OpI64TruncSatF64U:
		b.line("%s = %si64_trunc_sat_u(%s)", b.slotNames(s.Dst, wasm.I64), b.pfx(), a)
		return nil
	case wasm.OpI64TruncF32S, wasm.OpI64TruncF64S:
		// pfx, like the 32-bit forms: in exact mode the operand may be a boxed
		// NaN, and a table reaching the range compare raises a Lua error where
		// the spec wants a wasm trap.
		b.line("%s = %si64_trunc_s(%s)", b.slotNames(s.Dst, wasm.I64), b.pfx(), a)
		return nil
	case wasm.OpI64TruncF32U, wasm.OpI64TruncF64U:
		b.line("%s = %si64_trunc_u(%s)", b.slotNames(s.Dst, wasm.I64), b.pfx(), a)
		return nil
	case wasm.OpI64ReinterpretF64:
		b.line("%s = %sf64_to_bits(%s)", b.slotNames(s.Dst, wasm.I64), b.pfx(), a)
		return nil

	// -- i32 memory ---------------------------------------------------------
	//
	// The inlined load, at -opt=3 only. A call to ld32 costs 40.9 ns; the same
	// body expanded here costs 27.0, and of the 13.9 ns difference every bit is
	// the call itself -- the bounds check is 20% of a load and is kept.
	//
	// t0 holds the address so it is evaluated ONCE. It is a pre-declared
	// scratch, which is what keeps this legal under Invariant B: nothing may
	// declare a local after the first ::label::, and a `local` here would make
	// every goto past it illegal.
	//
	// The unaligned case still calls ld32 rather than being expanded too. It is
	// rare in real guest output -- LLVM aligns what it can -- and expanding it
	// would triple the size of every load for a path almost nothing takes.
	//
	// When the congruence analysis PROVES the effective address a multiple of
	// four, the modulo, the compare and the branch all go and the load is a
	// single table index. The bounds check stays: alignment says nothing about
	// range, and a negative address that happens to be a multiple of four would
	// otherwise index the table at a negative key and read nil.
	case wasm.OpI32Load:
		if g, ok := b.lgAccess[i]; ok {
			b.emitGuardedLoad32(g, i, d, a, s.Instr.MemOffset)
			return nil
		}
		b.line("t0 = %s", addrExpr(a, s.Instr.MemOffset))
		// The static fold. A constant address folds to a constant shard AND a
		// constant word index, so the whole select disappears; the bounds check
		// stays because MEMSIZE is a runtime quantity.
		if ref, ok := b.staticFold(i, s.Instr.MemOffset, 4); ok {
			b.line("if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end")
			b.line("%s = %s", d, ref)
			return nil
		}
		if b.al.AddrDividesBy(i, 4) {
			// Alignment proven, so there is no call in the slow arm and the
			// tail is ONE shared expression -- the no-else form applies and
			// this costs exactly what the flat load cost. See shardSlow.
			b.line("t1 = S1")
			b.line("if %s then", shardSlow(4))
			b.shardRebase("t1", "t1", 4)
			b.line("end")
			b.line("%s = t1[t0 / 4 + 1]", d)
			return nil
		}
		// Alignment unknown: the slow arm can end in a CALL to ld32, so there
		// are two tails and the if/else stays. The jump it costs is paid only
		// on a path that was going to be a call anyway.
		b.line("if %s then %s = S1[t0 / 4 + 1] else", shardFast(4, false), d)
		b.indent++
		b.line("if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end")
		b.line("if t0 %% 4 == 0 then t1 = t0 %% %d %s = %s", shardBytes, d, shardSlowRef("t1"))
		b.line("else %s = ld32(MEM, MEMSIZE, t0) end", d)
		b.indent--
		b.line("end")
		return nil

	// -- f64 memory ---------------------------------------------------------
	//
	// The inlined f64 load, at -opt=3 only. It reaches emitStep at all only
	// because stepExpr refuses to hand it over as an expression at this level.
	//
	// ld_f64 already reads the aligned pair itself rather than through two
	// ld32s -- that was worth 1.48x on `dot`. What is left is the CALL, which
	// the load-cost breakdown puts at 34%, and then a second one to ldexp.
	// This buys the first of the two.
	//
	// The fast path handles a NORMAL double only: e in (0, 2047). Zero,
	// subnormals, infinities and NaNs fall back to the helper, which keeps the
	// inlined arithmetic to one straight line and -- the part that matters --
	// keeps exact-NaN mode correct for free, since every value that could be a
	// BOXED NaN takes the fallback to xld_f64.
	//
	// t0..t3 are pre-declared scratches (see scratchCount): Invariant B forbids
	// a `local` here, and a bare assignment would be a global write.
	case wasm.OpF64Load:
		if g, ok := b.lgAccess[i]; ok {
			b.emitGuardedLoadF64(g, i, d, a, s.Instr.MemOffset)
			return nil
		}
		// An 8-byte aligned access CAN straddle a shard boundary, so the else
		// arm delegates whole rather than inlining a second copy of the
		// straddle rule. What used to be the unaligned fallback now also
		// carries the out-of-range case and the above-shard-0 case: all three
		// are ld_f64's, and all three were already a call.
		b.line("t0 = %s", addrExpr(a, s.Instr.MemOffset))
		b.line("if %s then", shardFast(8, false))
		b.line("  t1 = t0 / 4 + 1 t2 = S1[t1 + 1] t1 = S1[t1]")
		b.line("  t3 = t2 %% %s", signMin)
		b.line("  t3 = (t3 - t3 %% 1048576.0) / 1048576.0")
		b.line("  if t3 > 0 and t3 < 2047 then")
		// PE[t3] is 2^(t3-1075) -- a table read and a multiply where this used
		// to call ldexp. The `t3 > 0 and t3 < 2047` above is exactly the range
		// PE is defined over, so the guard the fast path already needed is also
		// what makes the table read safe.
		b.line("    %s = (t2 >= %s and -1.0 or 1.0) * ((t2 %% 1048576.0) * %s + t1 + 4503599627370496.0) * PE[t3]",
			d, signMin, wrapMod)
		// The two words are already in hand, so the non-normal fallback
		// converts them where it used to re-read them through a byte address.
		// That also removes the one place a within-shard index would have had
		// to be turned back into an absolute address.
		b.line("  else %s = %sbits_to_f64(t1, t2) end", d, b.pfx())
		b.line("else %s = %sld_f64(MEM, MEMSIZE, t0) end", d, b.pfx())
		return nil

	// -- i64 memory ---------------------------------------------------------
	case wasm.OpI64Load:
		if b.inlineLoads() {
			// One bounds check for the 8-byte range and the aligned pair read
			// here. The two-ld32 form below checks twice and calls twice; a
			// load has no partial effect, so folding the two checks into one
			// leading check is observationally identical.
			// The two ld32s in the else arm each select their own shard, so the
			// straddle needs nothing here: it is two independent 4-byte
			// accesses, each of which is wholly inside one shard.
			b.line("t0 = %s", addrExpr(a, s.Instr.MemOffset))
			b.line("if %s then t1 = t0 / 4 + 1 %s = S1[t1], S1[t1 + 1]",
				shardFast(8, false), b.slotNames(s.Dst, wasm.I64))
			b.line("else %s = ld32(MEM, MEMSIZE, t0), ld32(MEM, MEMSIZE, t0 + 4) end",
				b.slotNames(s.Dst, wasm.I64))
			return nil
		}
		b.line("%s = ld32(MEM, MEMSIZE, %s), ld32(MEM, MEMSIZE, %s)",
			b.slotNames(s.Dst, wasm.I64),
			addrExpr(a, s.Instr.MemOffset), addrExpr(a, s.Instr.MemOffset+4))
		return nil
	case wasm.OpI64Load32U:
		b.line("%s = ld32(MEM, MEMSIZE, %s), 0",
			b.slotNames(s.Dst, wasm.I64), addrExpr(a, s.Instr.MemOffset))
		return nil
	case wasm.OpI64Load32S:
		b.line("t0 = ld32(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset))
		b.line("%s = t0, t0 >= 2147483648.0 and 4294967295.0 or 0", b.slotNames(s.Dst, wasm.I64))
		return nil
	case wasm.OpI64Load8U:
		b.line("%s = ld8(MEM, MEMSIZE, %s), 0",
			b.slotNames(s.Dst, wasm.I64), addrExpr(a, s.Instr.MemOffset))
		return nil
	case wasm.OpI64Load16U:
		b.line("%s = ld16(MEM, MEMSIZE, %s), 0",
			b.slotNames(s.Dst, wasm.I64), addrExpr(a, s.Instr.MemOffset))
		return nil
	case wasm.OpI64Load8S:
		b.line("t0 = ld8(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset))
		b.line("t0 = t0 >= 128 and t0 + 4294967040.0 or t0")
		b.line("%s = t0, t0 >= 2147483648.0 and 4294967295.0 or 0", b.slotNames(s.Dst, wasm.I64))
		return nil
	case wasm.OpI64Load16S:
		b.line("t0 = ld16(MEM, MEMSIZE, %s)", addrExpr(a, s.Instr.MemOffset))
		b.line("t0 = t0 >= 32768 and t0 + 4294901760.0 or t0")
		b.line("%s = t0, t0 >= 2147483648.0 and 4294967295.0 or 0", b.slotNames(s.Dst, wasm.I64))
		return nil
	case wasm.OpI64Store:
		// One st64 rather than two st32s: an 8-byte access is bounds-checked as
		// a whole, so an out-of-range store leaves memory untouched as the spec
		// requires instead of landing the low word and then trapping.
		//
		// The inlined form keeps that ordering exactly -- the single check runs
		// before either word is written -- and only removes the call. An i64
		// value operand is a (lo, hi) pair, which forwarding never substitutes
		// an expression into, so there is nothing here to evaluate twice.
		if b.inlineWideStores() {
			// `t0 + 8 <= SHBOUND` proves BOTH words inside shard 0, so the fast
			// arm cannot straddle. The else arm hands the whole store to st64,
			// which owns the straddle and, with it, the spec's rule that an
			// out-of-range store leaves memory untouched -- one copy of that
			// rule, in the runtime, where it is tested.
			b.line("t0 = %s", addrExpr(a, s.Instr.MemOffset))
			b.line("if %s then t1 = t0 / 4 + 1 S1[t1] = %s %% %s S1[t1 + 1] = %s %% %s",
				shardFast(8, false),
				b.slotName(s.Args[1]), wrapMod, b.slotName(s.Args[1]+1), wrapMod)
			b.line("else st64(MEM, MEMSIZE, t0, %s) end", b.slotNames(s.Args[1], wasm.I64))
			return nil
		}
		b.line("st64(MEM, MEMSIZE, %s, %s)",
			addrExpr(a, s.Instr.MemOffset), b.slotNames(s.Args[1], wasm.I64))
		return nil
	case wasm.OpI64Store8:
		b.line("st8b(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64Store16:
		b.line("st16(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), b.slotName(s.Args[1]))
		return nil
	case wasm.OpI64Store32:
		b.line("st32(MEM, MEMSIZE, %s, %s)", addrExpr(a, s.Instr.MemOffset), b.slotName(s.Args[1]))
		return nil

	default:
		return fmt.Errorf("luagen: no lowering for %s in function %q", s.Op, f.Name)
	}
	return nil
}

// lowMask reports whether k is 2^n - 1, so `and` becomes `% 2^n`.
func lowMask(k uint32) (n uint32, ok bool) {
	if k == 0 || k == 0xFFFFFFFF {
		return 0, false
	}
	if k&(k+1) != 0 {
		return 0, false
	}
	for n = 0; n < 32; n++ {
		if k == (1<<n)-1 {
			return n, true
		}
	}
	return 0, false
}

// highMask reports whether k is ^(2^n - 1), so `and` becomes an align-down.
func highMask(k uint32) (n uint32, ok bool) {
	inv := ^k
	if inv == 0 {
		return 0, false
	}
	n, ok = lowMask(inv)
	if !ok || n == 0 {
		return 0, false
	}
	return n, true
}

// resultList names the slots a call's results land in, sized by type.
func (b *builder) resultList(s ir.Step) string {
	base := s.Dst
	var names []string
	for _, rt := range s.ResultTypes {
		names = append(names, b.slotNames(base, rt))
		base += ir.Slot(rt.Slots())
	}
	if len(names) == 0 {
		return b.slotName(s.Dst)
	}
	return strings.Join(names, ", ")
}

// callArgs expands each argument to every Lua value it occupies. A forwarded
// operand is already an expression and is passed through untouched, which is
// only ever the case for single-slot types.
func (b *builder) callArgs(s ir.Step, fwd []string) []string {
	var out []string
	for k := range s.Args {
		if s.ArgTypes[k].Slots() == 1 {
			out = append(out, fwd[k])
			continue
		}
		out = append(out, b.slotNames(s.Args[k], s.ArgTypes[k]))
	}
	return out
}

// inlineLoads reports whether an i32 load is expanded at its use site instead
// of calling ld32.
//
// -opt=3 only, because it is a TRADE and not a strict improvement: the load
// stops being an expression, so -opt=2's forwarding can no longer fold it into
// a larger one, and every load grows from one line to three in a chunk Factorio
// has to parse. It buys the call, which is 34% of a load's cost.
func (b *builder) inlineLoads() bool { return b.opt >= analysis.O3 }

// inlineStores reports whether an i32 store is expanded at its use site instead
// of calling st32.
//
// -opt=3 only, for the same reason as the load and with the same trade. A store
// is already a statement, so nothing about expression folding is given up here;
// what it costs is size, four lines where there was one.
func (b *builder) inlineStores() bool { return b.opt >= analysis.O3 }

// emitInlineStore32 expands i32.store at its use site: the mirror of the
// inlined load, with the store's own two extra obligations.
//
// The shape is st32's body, minus the call:
//
//	t0 = <address>
//	t1 = <value>                                -- only when it is not a bare name
//	if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
//	if MEMDIRTY and <t0's span left the cached page> then MEMPACK.mark(t0, t0 + 3) end
//	if t0 % 4 == 0 then MEM[t0 / 4 + 1] = t1 % 2^32 else st32(..., t0, t1) end
//
// **Operand order and single evaluation.** t0 and t1 are pre-declared scratch
// registers, which is what keeps this legal under Invariant B -- a `local` after
// the first `::label::` makes every goto past it illegal. They also make each
// operand evaluated exactly ONCE, in wasm's order: address, then value, then the
// access. The value is left in place when it is a bare name or numeral, because
// then naming it twice is free; anything composite goes through t1 rather than
// being printed into both arms. Neither operand can trap here -- a store's own
// lowering traps, so the peephole's one-trap-per-expression rule has already
// refused to forward a trapping operand into either position -- so moving the
// bounds check after them cannot change WHICH trap a guest sees.
//
// **THE DIRTY-PAGE MARK IS NOT OPTIONAL.** Under --persist=packed, the page set
// is what the next flush repacks. Every store in the system funnels through
// st8b/st16/st32 precisely so that no store can miss it; inlining st32 means
// inheriting that duty rather than escaping it. A store that does not mark its
// page is not a slow store, it is a store silently absent from the save: the
// guest runs correctly all session and comes back with stale memory, which in a
// lockstep multiplayer game is a desync and not an error message. It is emitted
// unconditionally and gated on MEMDIRTY exactly as st32 gates it, so this stays
// correct in every persistence mode without the emitter having to know which one
// is in force -- a coupling that would otherwise break the day `arm` is reachable
// from somewhere new.
//
// The `t0 < DPLO or t0 + 3 > DPHI` half is not a second correctness condition,
// it is st32's own fast path printed here: DPLO/DPHI bound the page most
// recently marked, so a store that stays inside it has nothing to add to the
// set. Getting that test wrong in the conservative direction costs a call;
// getting it wrong in the other direction is the desync above, which is why it
// is copied from the prelude verbatim rather than re-derived.
//
// **The `% 2^32` on the aligned path is kept**, for two reasons that are not the
// same reason. It makes the inlined form reduce its value operand exactly as the
// call did, so nothing about wrap deferral has to be re-reasoned against a second
// lowering; and MEM is required to hold genuine u32 words, because packed mode
// feeds them to string.pack("<I4"), which raises on anything else.
//
// The unaligned case still calls st32, exactly as the inlined load still calls
// ld32: LLVM aligns what it can, and expanding the byte-wise path would multiply
// the size of every store for something almost nothing takes.
func (b *builder) emitInlineStore32(s ir.Step, fw *forwarding, i int, addr, val string) {
	if g, ok := b.lgAccess[i]; ok {
		b.emitGuardedStore32(g, s, fw, i, addr, val)
		return
	}
	b.line("t0 = %s", addrExpr(addr, s.Instr.MemOffset))
	v := val
	if !fw.dupable[i][1] {
		b.line("t1 = %s", fw.raw[i][1])
		v = "t1"
	}
	// The static fold. Nothing about the mark changes: the page set indexes
	// byte addresses and a page never straddles a shard, so it is the same line
	// over the same span.
	if ref, ok := b.staticFold(i, s.Instr.MemOffset, 4); ok {
		b.line("if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end")
		b.line("if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end")
		b.line("%s = %s %% %s", ref, v, wrapMod)
		return
	}
	// THE MARK IS EMITTED IN BOTH ARMS RATHER THAN HOISTED IN FRONT OF THE
	// TEST, and that is not tidiness. Hoisting it would mark a page for a store
	// that is about to trap -- and for a NEGATIVE address, which floors to a
	// negative page number that reaches DPQ, the flush and `storage`. The
	// bounds check has always come first here; the merged form keeps it first
	// on both paths, at the price of one duplicated line on the cold arm.
	b.line("if %s then", shardFast(4, b.al.AddrDividesBy(i, 4)))
	b.indent++
	b.line("if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end")
	b.line("S1[t0 / 4 + 1] = %s %% %s", v, wrapMod)
	b.indent--
	b.line("else")
	b.indent++
	b.line("if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end")
	b.line("if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end")
	b.line("if t0 %% 4 == 0 then %s = %s %% %s else st32(MEM, MEMSIZE, t0, %s) end",
		shardSlowRefNoTmp(), v, wrapMod, v)
	b.indent--
	b.line("end")
}

// inlineWideStores reports whether an 8-byte STORE is expanded at its use site
// instead of calling st64/st_f64.
//
// Three conditions, and only the first is an optimization decision.
//
// `--persist=packed` tracks a dirty-page set (MEMPACK.mark/DPLO/DPHI in the
// prelude), and the property that makes it sound is that EVERY store in the
// system funnels through st8b/st16/st32/st64. An inlined store writes MEM
// directly and would walk straight past the marking: the bytes land in the live
// table, the flush never learns the page changed, and the value is silently
// missing from the save -- a corruption that appears one save/load cycle after
// the code that caused it. Rather than duplicate the marking rule into
// generated code and hope the two copies stay in step, the inlined store is
// simply not available in the one mode that needs the funnel.
//
// The page set made the marking cheaper to express -- one call behind a
// two-compare test rather than four inline compares -- and that does NOT reopen
// this. The objection was never the line count; it is that a second copy of the
// rule is a second place to forget it. The 4-byte store carries one because it
// is the store that dominates real guests, and widening that exception wants
// its own measurement rather than an inference from a cheaper mark.
//
// --gc=collected IS THAT SECOND CONSUMER, and it is why the gate now takes two
// modes instead of one. The incremental collector's write barrier is not a new
// mechanism: it is this same dirty-page set, armed only while a collection is
// marking (agents/gc.md section 5). Every writer already maintains it in every
// mode -- the helpers test MEMDIRTY, the inlined 4-byte store emits the mark
// line, the loop guard hoists one mark over its whole proven span -- and an
// audit of the emitted chunk found exactly one exception, this one. Armed in
// table mode against an ungated wide store, a collection would silently miss
// every i64/f64 write, and a missed mark is now worse than stale memory: it is
// a live object the sweep reclaims, i.e. a use-after-free inside a lockstep
// simulation.
//
// Stage A measured both fixes and they were indistinguishable -- emitting the
// mark line into the wide store cost at or below the A/A floor, and this gate
// cost the same except real_names at 1.013x. The tie is broken by the argument
// fk_rt.lua already makes at the MEMDIRTY declaration: an invariant maintained
// in two places is an invariant that drifts. Gating also has a property the
// other fix does not -- a guest that does not opt in emits a chunk that is
// bit-identical to today's, because this is a compile-time flag.
//
// Loads have no such constraint: nothing about reading memory is recorded.
func (b *builder) inlineWideStores() bool {
	return b.opt >= analysis.O3 &&
		b.opts.Persist != PersistPacked &&
		b.opts.GC != GCCollected
}

// pfx is the helper-name prefix for the current NaN mode: exact-mode helpers
// are the same names with an "x" in front.
func (b *builder) pfx() string {
	if b.exact() {
		return "x"
	}
	return ""
}

// isNonCanonicalNaN32 reports a NaN whose bits differ from the one canonical
// value a plain Lua number can represent. Only those need boxing.
func isNonCanonicalNaN32(bits uint32) bool {
	return math.IsNaN(float64(math.Float32frombits(bits))) && bits != 0x7FC00000
}

func isNonCanonicalNaN64(bits uint64) bool {
	return math.IsNaN(math.Float64frombits(bits)) && bits != 0x7FF8000000000000
}

// f32Literal renders an f32 constant from its raw IEEE bits.
//
// Hex float form is exact and round-trips; decimal would not. Factorio's
// tostring gives only 14 significant digits, so a decimal literal silently
// loses precision.
func f32Literal(bits uint32) string {
	return floatLiteral(float64(math.Float32frombits(bits)))
}

func f64Literal(bits uint64) string {
	return floatLiteral(math.Float64frombits(bits))
}

func floatLiteral(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "HUGE"
	case math.IsInf(v, -1):
		return "-HUGE"
	case math.IsNaN(v):
		return "NAN"
	case v == 0 && math.Signbit(v):
		return "-0.0"
	}
	return strconv.FormatFloat(v, 'x', -1, 64)
}

// addrExpr builds an effective address. The static offset is added in INFINITE
// precision per spec -- an out-of-range sum traps rather than wrapping -- so it
// never needs masking, and a zero offset costs nothing.
func addrExpr(base string, off uint32) string {
	if off == 0 {
		return base
	}
	return fmt.Sprintf("%s + %s", base, u32(off))
}

// emitBrTable lowers br_table, tiered on the number of DISTINCT targets rather
// than the number of entries: a switch statement compiled by any real toolchain
// produces a table with hundreds of entries and a handful of destinations.
func emitBrTable(b *builder, f *ir.Func, si int, s ir.Step, idx string, emitBranch func(ir.Branch)) error {
	if len(s.Targets) <= brTableChainLimit {
		for i, br := range s.Targets {
			b.line("if %s == %d then", idx, i)
			b.indent++
			emitBranch(br)
			b.indent--
			b.line("end")
		}
		emitBranch(s.Default)
		return nil
	}

	// Index -> small group id through the hoisted constant array, then dispatch
	// on the id. A nil result doubles as the free out-of-range default, and the
	// array is 1-based so it stays out of Lua's hash part.
	b.line("t0 = BT[%q][%s + 1]", branchTableID(f.Index, si), idx)
	b.line("if t0 == nil then")
	b.indent++
	emitBranch(s.Default)
	b.indent--
	b.line("end")
	for gi, br := range branchTableGroups(s) {
		b.line("if t0 == %d then", gi+1)
		b.indent++
		emitBranch(br)
		b.indent--
		b.line("end")
	}
	return nil
}

// upvalueMargin is how many chunk-level locals promotion leaves unspent.
//
// checkChunkLocals refuses the module when the chunk goes past Lua's 200, and a
// module that compiled yesterday must not start failing because a hot callee
// took the last slot. The margin also absorbs a prelude that grows by a name or
// two without turning that into a compile error for someone else's guest.
const upvalueMargin = 4

// maxUpvalues caps promotion regardless of headroom.
//
// Well below the theoretical ceiling: the prelude alone declares 167 of Lua's
// 200 chunk locals, so the real headroom on a guest with one global is around
// 25 -- the "~120" the M5 plan budgeted was written before the prelude reached
// its current size. The cap is here so the number never becomes the surprising
// part of a chunk-local overflow.
const maxUpvalues = 120

// trailingChunkLocals is what the emitter still declares at chunk scope AFTER
// promotion has already chosen how many names to spend.
//
// Today that is `local exports` and nothing else. It has to be a constant
// because the text does not exist yet when upvalueBudget runs -- and a constant
// that drifts silently would eat the margin, which is the one thing the margin
// exists to prevent. TestPromotionLeavesTheMarginItPromises pins the landed
// count, so adding a trailing local fails a test rather than costing a slot.
const trailingChunkLocals = 1

// upvalueBudget is how many callees may be promoted before the chunk runs out
// of locals.
func upvalueBudget(b *builder, m *ir.Module) int {
	spent := countChunkLocals(b.sb.String()) + trailingChunkLocals
	free := maxChunkLocals - spent - upvalueMargin
	if free > maxUpvalues {
		free = maxUpvalues
	}
	if free < 0 {
		return 0
	}
	return free
}

// emitUpvalueDecls declares the promoted names, BEFORE any function body that
// will reference them.
//
// Order is the whole trick. A Lua name resolves to an upvalue only if a `local`
// for it is already in scope where the closure is created; declare it after and
// every reference silently becomes a global read, which is slower than the
// table lookup promotion was meant to replace and also wrong, since the global
// table is shared with the rest of the mod.
func emitUpvalueDecls(b *builder, m *ir.Module) {
	if !b.opt.Upvalues() {
		return
	}
	hot := analysis.HotCallees(&ir.Module{Funcs: m.Funcs}, upvalueBudget(b, m))
	if len(hot) == 0 {
		return
	}
	b.up = map[uint32]string{}
	names := make([]string, 0, len(hot))
	for _, fi := range hot {
		n := upvalName(fi)
		b.up[fi] = n
		names = append(names, n)
	}
	b.line("-- hot callees promoted to upvalues: F[idx](...) costs 21.32 ns, an")
	b.line("-- upvalue call 16.82 ns, measured in Factorio 2.0.77")
	b.line("local %s", strings.Join(names, ", "))
}

// emitUpvalueBindings fills the promoted names in, after every definition
// exists.
//
// After, not before: F[n] is assigned as the functions are emitted, and a
// module may call forwards. Binding at the end means a recursive callee sees
// its own upvalue already set by the time anything runs.
func emitUpvalueBindings(b *builder, m *ir.Module) {
	if len(b.up) == 0 {
		return
	}
	idx := make([]uint32, 0, len(b.up))
	for fi := range b.up {
		idx = append(idx, fi)
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	for _, fi := range idx {
		b.line("%s = F[%d]", b.up[fi], fi)
	}
	b.blank()
}

// unwind is the frame-stack release that has to precede every return, or the
// empty string when the function did not spill.
//
// Before the returned expression rather than after, because Lua requires
// `return` to be a block's last statement. That is safe even when the
// expression reads a spilled slot: FP is a bump pointer and nothing clears what
// is above it, so FS[fb+k] still holds the value after FP has moved back.
func (b *builder) unwind() string {
	if !b.sp.Active() {
		return ""
	}
	return "FP = fb "
}

// emitFrameStack declares the chunk-level frame stack, when any function spills.
//
// Two chunk locals rather than one table with a field, because the chunk-local
// budget is tight but a field read is OP_GETUPVAL + OP_GETTABLE on the hottest
// path a spilled function has.
func emitFrameStack(b *builder) {
	if !b.spilled {
		return
	}
	b.line("-- frame stack: slots past Lua's per-function local limit live here,")
	b.line("-- indexed off a per-call base. FP is reset at every entry point,")
	b.line("-- because a trap unwinds past the epilogue that would restore it.")
	b.line("local FS, FP = {}, 0")
}
