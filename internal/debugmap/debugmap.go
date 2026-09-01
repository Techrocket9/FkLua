// Package debugmap builds the sidecar document that says which guest function
// each line of a packaged fk_module.lua came from.
//
// A guest's program is not in the Lua state, so a Factorio stack frame or an
// error location names generated Lua and nothing else: "fk_module.lua:4211" is
// true and says nothing about the program that was written. The map closes that
// gap at FUNCTION granularity. Given a line, a consumer binary-searches the
// ranges and gets back the wasm function index, the function's name, and -- when
// the guest carries DWARF -- the source file and line it was defined at.
//
// What it deliberately is not: it does not move execution, it does not describe
// statements, and it never claims a Lua line IS a source line. The Lua location
// stays the real location; the map only says whose code that is.
//
// The name-only form is the guaranteed baseline. DWARF is enrichment and every
// failure to read it is silent by design: a map with no src/line is useful, and
// a packaging that refused because a guest was built without debug info would
// be a regression for every project that never asked for one.
package debugmap

import (
	"debug/dwarf"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Version is the format version, the document's first field. A consumer reads
// it before anything else and must ignore fields it does not know, so adding a
// field is not a version bump; changing what an existing one means is.
const Version = 1

// Map is the whole document. Field order here is the emitted key order, which
// is what makes the bytes stable: encoding/json writes struct fields in
// declaration order, and nothing in this document is a Go map.
type Map struct {
	Version   int    `json:"fklua_map"`
	Module    string `json:"module"`
	Functions []Func `json:"functions"`
}

// Func is one defined wasm function and where its Lua lives.
type Func struct {
	// Lua is the 1-based inclusive line range in the PACKAGED module file,
	// wrapper included. Ranges are ascending and do not overlap.
	Lua [2]int `json:"lua"`
	// Wasm is the canonical wasm function index, imports counted first.
	Wasm uint32 `json:"wasm"`
	// Name is the best-effort human name: the name-section name, demangled when
	// it was a Rust v0 symbol, or FkLua's synthetic func[N] when the binary
	// carries no name for it.
	Name string `json:"name"`
	// Mangled is the raw name-section name, and it is present only when
	// demangling changed it. A consumer that wants to grep a linker map or a
	// profile still has the symbol; a consumer that wants to show a person
	// something reads Name and never sees this field at all.
	Mangled string `json:"mangled,omitempty"`
	// Src and Line come from DWARF and are absent together. Emitted only when
	// both are known and the line is non-zero: a file with line 0 is DWARF
	// saying "somewhere in here", which is not something to put in front of a
	// reader as a location.
	Src  string `json:"src,omitempty"`
	Line int    `json:"line,omitempty"`
}

// JSON renders the document: two-space indent and a trailing newline, which is
// what every other JSON this compiler writes looks like. The bytes are stable
// for stable input -- struct fields encode in declaration order and no map
// enters the document -- so a repackaging of the same guest produces the same
// file, and so a --zip archive stays reproducible.
func (m Map) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WithSource counts the entries DWARF could place in a source file. Zero is the
// ordinary answer for a guest built without debug info, and it is the number
// worth putting in front of an author, because the remedy is a build flag.
func (m Map) WithSource() int {
	n := 0
	for _, f := range m.Functions {
		if f.Src != "" {
			n++
		}
	}
	return n
}

// Build assembles the map for one emitted module.
//
// module is the packaged LUA file the ranges are measured in -- fk_module.lua,
// not the map's own name -- so a consumer holding a stack frame can tell at a
// glance whether this map is the one that describes it. lineOffset is
// how many lines the packaging wrapper prepends to the chunk, and src is the
// decoded guest -- nil is allowed and produces the name-only baseline. There is
// no error return because there is no failure that should stop a packaging: a
// guest with no DWARF, unreadable DWARF or half-readable DWARF all produce a
// map, with fewer fields filled in.
func Build(module string, spans []luagen.FuncSpan, lineOffset int, src *wasm.Module) Map {
	m := Map{Version: Version, Module: module, Functions: make([]Func, 0, len(spans))}
	lines := sourceLines(src)
	for _, s := range spans {
		name, mangled := Demangle(s.Name)
		f := Func{
			Lua:  [2]int{s.Start + lineOffset, s.End + lineOffset},
			Wasm: s.Index,
			Name: name,
		}
		if mangled != "" {
			f.Mangled = mangled
		}
		if sl, ok := lines[s.Index]; ok && sl.line > 0 && sl.file != "" {
			f.Src, f.Line = sl.file, sl.line
		}
		m.Functions = append(m.Functions, f)
	}
	// Ascending by first line, which is the order a consumer binary-searches.
	// It is already the order the emitter produced -- functions are emitted in
	// wasm index order and nothing reorders them -- so this sort is a guarantee
	// rather than a transformation.
	sort.SliceStable(m.Functions, func(i, j int) bool {
		return m.Functions[i].Lua[0] < m.Functions[j].Lua[0]
	})
	return m
}

// srcLine is one function's source position.
type srcLine struct {
	file string
	line int
}

// sourceLines resolves every defined function it can to a source file and line,
// by wasm function index. An empty result is the normal answer for a guest built
// without debug info, and it is also the answer whenever anything at all goes
// wrong: this is enrichment, and there is no failure here worth refusing a
// packaging over.
func sourceLines(m *wasm.Module) map[uint32]srcLine {
	if m == nil || len(m.CodeSpans) != len(m.Funcs) {
		return nil
	}
	d, ok := load(m)
	if !ok {
		return nil
	}
	// low_pc IS the body's offset within the code section payload, exactly --
	// see internal/wasm/codespans.go for the measurement. Exact equality rather
	// than a range test, because a subprogram that starts mid-body is an inlined
	// copy and belongs to whoever it was inlined into, not to the function it
	// happens to sit inside.
	byLow := make(map[uint64]uint32, len(m.CodeSpans))
	for i, s := range m.CodeSpans {
		byLow[uint64(s.Lo)] = m.Funcs[i].Index
	}

	out := map[uint32]srcLine{}
	r := d.Reader()
	var cu *cuState
	for {
		e, err := r.Next()
		if err != nil || e == nil {
			// A truncated or unreadable .debug_info leaves whatever was already
			// resolved in place. Half a map is worth more than none, and the
			// half that exists was read the same way as the whole.
			break
		}
		switch e.Tag {
		case dwarf.TagCompileUnit:
			cu = newCU(d, e)
			continue
		case dwarf.TagSubprogram:
		default:
			continue
		}
		low, hasLow := e.Val(dwarf.AttrLowpc).(uint64)
		if !hasLow || cu == nil {
			// No low_pc means an abstract or declared-only subprogram: a
			// template for inlined copies, with no code of its own.
			continue
		}
		idx, ok := byLow[low]
		if !ok {
			continue
		}
		if _, seen := out[idx]; seen {
			continue
		}
		if sl, ok := cu.position(e, low); ok {
			out[idx] = sl
		}
	}
	return out
}

// load hands debug/dwarf the module's DWARF sections. Both toolchains emit
// DWARF 4 today; the version-5 side tables are offered anyway, because a
// toolchain that starts emitting them should degrade to fewer answers rather
// than to a parse error.
func load(m *wasm.Module) (*dwarf.Data, bool) {
	sec := func(name string) []byte {
		b, _ := m.CustomSectionByName(name)
		return b
	}
	if len(sec(".debug_info")) == 0 || len(sec(".debug_abbrev")) == 0 {
		return nil, false
	}
	d, err := dwarf.New(
		sec(".debug_abbrev"), sec(".debug_aranges"), sec(".debug_frame"),
		sec(".debug_info"), sec(".debug_line"), sec(".debug_pubnames"),
		sec(".debug_ranges"), sec(".debug_str"))
	if err != nil {
		return nil, false
	}
	for _, extra := range []string{
		".debug_line_str", ".debug_str_offsets", ".debug_addr",
		".debug_rnglists", ".debug_loclists",
	} {
		if b := sec(extra); len(b) > 0 {
			_ = d.AddSection(extra, b)
		}
	}
	return d, true
}

// cuState is one compilation unit and the two things a position needs from it:
// its file table and its line program.
//
// The line reader is built once per unit and reused. dwarf.LineReader.SeekPC
// rewinds on its own when asked for an earlier address, so one reader answers
// every subprogram in the unit; building one per subprogram re-parses the whole
// line program each time, which on a real guest is thousands of redundant
// passes.
type cuState struct {
	compDir string
	files   []*dwarf.LineFile
	lr      *dwarf.LineReader
}

func newCU(d *dwarf.Data, e *dwarf.Entry) *cuState {
	cu := &cuState{}
	cu.compDir, _ = e.Val(dwarf.AttrCompDir).(string)
	if lr, err := d.LineReader(e); err == nil && lr != nil {
		cu.lr = lr
		cu.files = lr.Files()
	}
	return cu
}

// position answers where a subprogram was declared.
//
// DW_AT_decl_file / DW_AT_decl_line first, because they are the declaration
// itself. When they are absent -- which is the whole of a Rust
// line-tables-only build, where there are no declaration attributes to read --
// the line program is asked what source line the function's first byte belongs
// to, which lands on the signature or the first statement. That fallback is
// what makes a line-tables-only guest carry source positions at all.
func (c *cuState) position(e *dwarf.Entry, low uint64) (srcLine, bool) {
	if df, ok := e.Val(dwarf.AttrDeclFile).(int64); ok {
		if dl, ok := e.Val(dwarf.AttrDeclLine).(int64); ok && dl > 0 {
			if df >= 0 && int(df) < len(c.files) && c.files[df] != nil {
				return srcLine{c.clean(c.files[df].Name), int(dl)}, true
			}
		}
	}
	if c.lr == nil {
		return srcLine{}, false
	}
	var le dwarf.LineEntry
	if err := c.lr.SeekPC(low, &le); err != nil || le.File == nil || le.Line <= 0 {
		return srcLine{}, false
	}
	return srcLine{c.clean(le.File.Name), le.Line}, true
}

// clean makes a DWARF path readable where it can and leaves it alone where it
// cannot.
//
// A path under the unit's compilation directory is emitted relative to it,
// which is what turns a build-machine absolute path into the project-relative
// one an author recognises. Measured: a Rust guest's own sources become
// "<crate>/src/lib.rs" and the standard library becomes
// "library/core/src/option.rs" (rustc compiles it with comp_dir "/rustc/<hash>").
// A TinyGo guest's units carry a comp_dir the source paths are not under, so
// its paths stay as DWARF wrote them, absolute. Rewriting further would mean
// guessing, and a guessed path is worse than a long one.
func (c *cuState) clean(p string) string {
	if p == "" || c.compDir == "" {
		return p
	}
	dir := strings.TrimSuffix(c.compDir, "/")
	if rest, ok := strings.CutPrefix(p, dir+"/"); ok && rest != "" {
		return rest
	}
	return p
}
