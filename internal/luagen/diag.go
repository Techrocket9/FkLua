package luagen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// NaNMode selects how faithfully NaN bit patterns are preserved.
type NaNMode int

const (
	// NaNCanonical is the default. A NaN is a plain Lua number, which cannot
	// carry a sign bit or a payload, so every NaN collapses to one canonical
	// value. Fast, and correct for every program that does not inspect NaN bits.
	NaNCanonical NaNMode = iota

	// NaNExact boxes any NaN carrying non-canonical bits into a table so its
	// sign and payload survive constants, memory and reinterpretation.
	// Substantially slower: every float operation has to check whether its
	// operands are boxed, so none of them can use a plain Lua operator.
	NaNExact
)

func (m NaNMode) String() string {
	if m == NaNExact {
		return "exact"
	}
	return "canonical"
}

// ParseNaNMode maps a flag value onto a mode.
func ParseNaNMode(s string) (NaNMode, error) {
	switch s {
	case "", "canonical", "fast":
		return NaNCanonical, nil
	case "exact", "strict":
		return NaNExact, nil
	}
	return 0, fmt.Errorf("unknown NaN mode %q (want \"canonical\" or \"exact\")", s)
}

// PersistMode selects how guest state survives a save.
//
// The mode is a COMPILE-TIME fact that travels in the generated module's
// `persist` table, rather than something a mod's control.lua is told. That is
// what keeps control.lua copied out verbatim: it ships as a complete runtime
// that can drive any mode, and the module says which one it was built for.
type PersistMode int

const (
	// PersistNone rebuilds guest memory from the data segments on every load.
	// Deterministic -- every client rebuilds identical bytes -- but nothing a
	// guest accumulated during play survives. This was the only behaviour
	// through M5, and it stays reachable because it is the right answer for a
	// stateless guest with a large heap, whose saves would otherwise carry
	// megabytes that mean nothing.
	PersistNone PersistMode = iota

	// PersistTable makes storage.fk_mem the live word table. Zero steady-state
	// overhead: a guest store lands directly in the structure Factorio
	// serializes, with no sync step at all. The cost is paid at save and at
	// multiplayer join, where the table is walked entry by entry.
	PersistTable

	// PersistPacked keeps the live word table OUTSIDE storage and mirrors it
	// into one string per 4 KiB page. Strings serialize and ship far better than
	// a table with one entry per word -- a 1 MiB heap becomes 256 strings rather
	// than 262,144 numbers -- at the cost of repacking whatever changed after
	// each guest call, plus a two-compare dirty-page test on every store.
	//
	// The right choice when the heap is large and writes are localised, which
	// is what a bump allocator with a working set produces.
	PersistPacked

	// PersistAuto asks the compiler to choose between table and packed from the
	// module's declared heap size. It is never emitted: ResolvePersist turns it
	// into one of the other two before code generation.
	PersistAuto
)

// AutoThresholdBytes is the declared heap size at or above which PersistAuto
// picks packed.
//
// The honest input to this decision is a guest's WRITE LOCALITY, which the
// compiler cannot know: packed costs ~40 us per dirty 4 KiB page per guest call,
// so a guest that scatters writes across a large heap pays far more than one
// with a working set, at identical heap sizes. Heap size is a proxy, and this
// constant is where the proxy was set rather than a measured optimum.
//
// The arithmetic behind the value, from the measurements in agents/guests.md:
// table mode costs 2.29 bytes of save per heap word, so a 1 MiB heap (262,144
// words) adds about 600 KB to every save and every multiplayer join. That is
// the point where the save-size cost stops being a footnote. Below it, table's
// bounded, save-time-only cost is the safer trade.
const AutoThresholdBytes = 1 << 20

// ResolvePersist turns a requested mode into the one that will actually be
// emitted, given the module's declared initial heap size in bytes.
//
// Split out from emission so a caller can REPORT the choice. An automatic
// decision the user cannot see is one they cannot correct.
func ResolvePersist(m PersistMode, heapBytes uint64) PersistMode {
	if m != PersistAuto {
		return m
	}
	if heapBytes >= AutoThresholdBytes {
		return PersistPacked
	}
	return PersistTable
}

func (m PersistMode) String() string {
	switch m {
	case PersistTable:
		return "table"
	case PersistPacked:
		return "packed"
	case PersistAuto:
		return "auto"
	}
	return "none"
}

// ParsePersistMode maps a flag value onto a mode.
func ParsePersistMode(s string) (PersistMode, error) {
	switch s {
	case "", "table":
		return PersistTable, nil
	case "packed":
		return PersistPacked, nil
	case "auto":
		return PersistAuto, nil
	case "none", "off":
		return PersistNone, nil
	}
	return 0, fmt.Errorf(
		"unknown persistence mode %q (want \"table\", \"packed\", \"auto\" or \"none\")", s)
}

// GCMode selects whether the guest's own heap is collected.
//
// It is a CODEGEN option and not only a build flag, and that surprised stage A
// as much as it will surprise anyone reading this. The collector itself is
// guest Go (guest/go/fkgc) compiled by this emitter like any other guest code,
// so it needs nothing here -- except that the write barrier stage C arms is
// MEMDIRTY, the dirty-page set --persist=packed already maintains, and there is
// exactly one writer of guest memory that does not maintain it. See
// inlineWideStores.
type GCMode int

const (
	// GCLeaking is the shipping default and the behaviour every guest has had
	// since M4: guest memory is an arena that only grows, and staying inside
	// it is the guest author's problem (agents/guests.md, "the guest heap
	// budget"). The zero value, so a caller that does not care gets the
	// behaviour that cannot surprise it.
	GCLeaking GCMode = iota

	// GCCollected means the guest was built with -gc=custom and imports
	// guest/go/fkgc, so its heap is collected.
	//
	// What it changes in the EMITTER is one thing: the inlined 8-byte store
	// stops being available, exactly as it already stops being available under
	// --persist=packed, because it is the one lowering that writes MEM without
	// marking its page. See inlineWideStores.
	GCCollected
)

func (m GCMode) String() string {
	if m == GCCollected {
		return "collected"
	}
	return "leaking"
}

// ParseGCMode maps a flag value onto a mode.
//
// "leaking" is spelled the way TinyGo spells it, because that is what the flag
// really selects and a guest author will see it again in the tinygo command
// line. "collected" is NOT spelled "custom": -gc=custom is TinyGo's name for a
// SEAM, and a guest that passes it without importing guest/go/fkgc does not
// link at all -- naming the seam here would advertise a flag that is only half
// of what it takes.
func ParseGCMode(s string) (GCMode, error) {
	switch s {
	case "", "leaking", "none", "off":
		return GCLeaking, nil
	case "collected", "custom":
		return GCCollected, nil
	}
	return 0, fmt.Errorf("unknown gc mode %q (want \"leaking\" or \"collected\")", s)
}

// Options control code generation.
type Options struct {
	NaN NaNMode

	// Opt is the optimization level. The zero value is analysis.O0, which
	// disables every pass and reproduces the M4 emitter byte for byte -- so a
	// caller that does not care gets the conservative answer, and every caller
	// that does care has to say so.
	Opt analysis.Level

	// Roots restricts which exports count as entry points for diagnostics. Empty
	// means every export, which is the only safe assumption for a bare compile:
	// an arbitrary host may call anything the module exports.
	//
	// A mod is different, and this is why the field exists. TinyGo exports its
	// libm alongside the guest's own entry points -- fmaximumf, fminimumf and
	// friends -- and a mod's control.lua wires none of them. Reporting a NaN
	// diagnostic in code the mod can never call names a function the author
	// never wrote and cannot reach, which is precisely the noise that teaches
	// people to ignore diagnostics.
	Roots []string

	// Persist selects how guest state survives a save. The zero value is
	// PersistNone, the pre-M6 behaviour, so a caller that does not care gets
	// the one that cannot surprise it with a larger save file.
	Persist PersistMode

	// GC selects whether the guest collects its own heap. The zero value is
	// GCLeaking, which is what every guest has had since M4 and what a chunk
	// emitted without an opinion should be.
	GC GCMode

	// Fuel is the number of loop back-edges one guest entry call may take
	// before it is stopped. Zero disables the check entirely.
	//
	// wasm has no instruction budget and a conforming module may loop forever.
	// This is a HOST policy, and it exists because the host is a lockstep game
	// with no way to interrupt a running mod: an infinite guest loop hangs
	// every player's client until they kill the process.
	Fuel int

	// BuildID identifies the guest build this module was compiled from, and is
	// stamped into the persistence surface.
	//
	// A save records the BuildID that wrote it. Any change to the guest moves
	// its heap layout -- static addresses shift, struct offsets move -- so a
	// heap written by one build and read by another is undefined. Comparing
	// this is what turns that from silent corruption into a decision.
	//
	// A "build" is WIDER THAN THE MODULE, and this layer deliberately does not
	// decide how much wider: it takes whatever string the packager computed.
	// cmd/fklua folds the resolved --api pin in beside the module's digest,
	// because the packaged member, event and define tables are pin-derived and
	// the guest heap depends on them -- see its buildID.
	//
	// Empty means "unidentified", which never matches a stamped save and so
	// fails toward not adopting.
	BuildID string
}

// Diagnostic reports a place where the emitted Lua cannot reproduce wasm
// semantics exactly.
//
// These are not errors: the overwhelming majority of programs never observe the
// difference. But they are also not nothing, and discovering them from a
// mismatched test result later is far worse than being told at compile time.
type Diagnostic struct {
	Func   string
	Op     string
	Count  int
	Detail string
	Remedy string
	// ReachedFrom names the entry points that can reach this function. It is
	// what makes the diagnostic actionable when the function belongs to the
	// guest's toolchain rather than to code the author wrote.
	ReachedFrom []string
}

// reachedFromPhrase renders the entry points, trimmed so a widely-reached
// helper does not bury the finding under a list.
func (d Diagnostic) reachedFromPhrase() string {
	switch n := len(d.ReachedFrom); {
	case n == 0:
		return ""
	case n <= 2:
		return ", reached from " + strings.Join(d.ReachedFrom, " and ")
	default:
		return fmt.Sprintf(", reached from %s and %d other entry points",
			d.ReachedFrom[0], n-1)
	}
}

func (d Diagnostic) String() string {
	s := fmt.Sprintf("%s in %q", d.Op, d.Func)
	if d.Count > 1 {
		s = fmt.Sprintf("%s in %q (%d times)", d.Op, d.Func, d.Count)
	}
	return fmt.Sprintf("%s%s:\n    %s", s, d.reachedFromPhrase(), d.Detail)
}

// Result is a compiled module plus anything worth telling the author about.
type Result struct {
	Lua         string
	Diagnostics []Diagnostic
}

// nanSensitive classifies an op by how it can expose a NaN's bits.
//
// A NaN's sign and payload are unobservable inside pure arithmetic -- the spec
// lets any operation produce any payload, and every comparison is false for a
// NaN either way. They become observable only where bits are read or written:
// reinterpretation, memory, and copysign, which lifts a sign bit off a NaN and
// puts it on a result that is not one.
func nanSensitive(op wasm.Op) (detail, remedy string, ok bool) {
	switch op {
	case wasm.OpF32Copysign, wasm.OpF64Copysign:
		return "a Lua number cannot carry a NaN's sign bit, so copysign with a NaN " +
				"second operand produces the wrong sign",
			"compile with --nan=exact if this operand can be a NaN", true

	case wasm.OpI32ReinterpretF32, wasm.OpF32ReinterpretI32:
		return "a Lua number cannot carry a NaN payload, so reinterpreting a " +
				"non-canonical NaN loses its bits",
			"compile with --nan=exact if NaN bit patterns matter here", true

	case wasm.OpF32Load, wasm.OpF64Load, wasm.OpF32Store, wasm.OpF64Store:
		return "a NaN loses its sign and payload on the way through memory; " +
				"every other value round-trips exactly",
			"compile with --nan=exact if NaN bit patterns matter here", true
	}
	return "", "", false
}

// reachedFrom maps each function index to the entry points that can reach it.
//
// Naming the entry point is what makes a diagnostic actionable. "fmaximumf
// reinterprets a NaN" names a function the guest author never wrote and cannot
// find; "reached from export fk_on_tick" tells them which of their own entry
// points is affected, which is a question they can actually answer.
//
// A function absent from the result is unreachable, and a diagnostic about code
// the guest never runs is worse than none -- it is the noise that teaches people
// to ignore the output.
//
// call_indirect is handled conservatively: a module containing one is assumed to
// reach every function in its element segments, since which entry a given call
// selects is a runtime value.
func reachedFrom(m *ir.Module, opts Options) map[uint32][]string {
	byIndex := make(map[uint32]*ir.Func, len(m.Funcs))
	for _, f := range m.Funcs {
		byIndex[f.Index] = f
	}

	// Roots are every export plus the start function, which runs at load
	// whether or not anything references it.
	type root struct {
		name  string
		index uint32
	}
	var roots []root
	for _, e := range m.Exports {
		if !isRoot(e.Name, opts.Roots) {
			continue
		}
		roots = append(roots, root{name: fmt.Sprintf("export %q", e.Name), index: e.FuncIndex})
	}
	if m.Source != nil && m.Source.Start >= 0 {
		roots = append(roots, root{name: "the start function", index: uint32(m.Source.Start)})
	}

	out := map[uint32][]string{}
	for _, r := range roots {
		seen := map[uint32]bool{r.index: true}
		queue := []uint32{r.index}
		for len(queue) > 0 {
			idx := queue[0]
			queue = queue[1:]
			out[idx] = append(out[idx], r.name)

			f := byIndex[idx]
			if f == nil {
				continue
			}
			visit := func(i uint32) {
				if !seen[i] {
					seen[i] = true
					queue = append(queue, i)
				}
			}
			for _, s := range f.Steps {
				switch s.Op {
				case wasm.OpCall:
					visit(s.Callee)
				case wasm.OpCallIndirect:
					if m.Source != nil {
						for _, seg := range m.Source.Elems {
							for _, fi := range seg.Funcs {
								visit(fi)
							}
						}
					}
				}
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// isRoot reports whether an export counts as an entry point. An empty allow-list
// means every export does.
func isRoot(name string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if a == name {
			return true
		}
	}
	return false
}

// Diagnose reports every NaN-sensitive operation in a module.
//
// In exact mode there is nothing to report: the boxing makes those operations
// faithful, which is the entire point of the mode.
func Diagnose(m *ir.Module, opts Options) []Diagnostic {
	if opts.NaN == NaNExact {
		return nil
	}

	live := reachedFrom(m, opts)

	type key struct{ fn, op string }
	seen := map[key]*Diagnostic{}

	for _, f := range m.Funcs {
		if f.Unsupported != nil {
			continue
		}
		// Dead code cannot produce a wrong answer, so it is not worth a warning.
		if len(live[f.Index]) == 0 {
			continue
		}
		for _, s := range f.Steps {
			detail, remedy, ok := nanSensitive(s.Op)
			if !ok {
				continue
			}
			k := key{f.Name, s.Op.String()}
			if d, exists := seen[k]; exists {
				d.Count++
				continue
			}
			seen[k] = &Diagnostic{
				Func: f.Name, Op: s.Op.String(), Count: 1,
				Detail: detail, Remedy: remedy, ReachedFrom: live[f.Index],
			}
		}
	}

	out := make([]Diagnostic, 0, len(seen))
	for _, d := range seen {
		out = append(out, *d)
	}
	// Stable order: by function, then by op, so output is diffable.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Func != out[j].Func {
			return out[i].Func < out[j].Func
		}
		return out[i].Op < out[j].Op
	})
	return out
}

// FormatDiagnostics renders diagnostics for a terminal, collapsing the repeated
// remedy into one closing line rather than restating it per finding.
func FormatDiagnostics(ds []Diagnostic) string {
	if len(ds) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d NaN-sensitive operation(s); results differ from the spec only\n", len(ds))
	b.WriteString("for NaN sign bits and payloads:\n")
	for _, d := range ds {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	b.WriteString("\nMost programs never observe this. If yours does, recompile with\n")
	b.WriteString("--nan=exact, which preserves NaN bits at a substantial speed cost.\n")
	return b.String()
}
