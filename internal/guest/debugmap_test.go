package guest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/debugmap"
	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// mapDoc is a CONSUMER's view of the document. Written out here rather than
// imported from internal/debugmap, because a tool outside this repository codes
// against these names and a test that unmarshalled the producer's own struct
// could not tell a rename from a no-op.
type mapDoc struct {
	Version   int    `json:"fklua_map"`
	Module    string `json:"module"`
	Functions []struct {
		Lua     [2]int `json:"lua"`
		Wasm    uint32 `json:"wasm"`
		Name    string `json:"name"`
		Mangled string `json:"mangled"`
		Src     string `json:"src"`
		Line    int    `json:"line"`
	} `json:"functions"`
}

// mapForGuest compiles a wasm through the real pipeline, packages it, and
// returns the map beside the packaged module's lines.
func mapForGuest(t *testing.T, wasmPath, modName string) (mapDoc, []byte, []string) {
	t.Helper()
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding the guest: %v", err)
	}
	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, spans, err := luagen.EmitModuleSpans(im, luagen.Options{})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	doc := debugmap.Build(factorio.GeneratedModuleFile, spans,
		factorio.ChunkLineOffset, im.Source)
	body, err := doc.JSON()
	if err != nil {
		t.Fatalf("rendering the map: %v", err)
	}
	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: modName, Version: "0.1.0", Title: modName, Author: "FkLua",
			FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk: src, MapJSON: string(body),
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	packaged, err := os.ReadFile(filepath.Join(dir, factorio.MapFile))
	if err != nil {
		t.Fatalf("the packaged mod has no map: %v", err)
	}
	var m mapDoc
	if err := json.Unmarshal(packaged, &m); err != nil {
		t.Fatalf("the map is not JSON: %v\n%s", err, packaged)
	}
	lua, err := os.ReadFile(filepath.Join(dir, factorio.GeneratedModuleFile))
	if err != nil {
		t.Fatal(err)
	}
	return m, packaged, strings.Split(string(lua), "\n")
}

// checkGuestMap is the whole claim, against a guest a real toolchain produced:
// every range brackets the function it names, in the file as packaged.
func checkGuestMap(t *testing.T, m mapDoc, lines []string, wantExport string) {
	t.Helper()
	if m.Version != 1 {
		t.Errorf("fklua_map = %d, want 1", m.Version)
	}
	if m.Module != factorio.GeneratedModuleFile {
		t.Errorf("module = %q, want %q", m.Module, factorio.GeneratedModuleFile)
	}
	if len(m.Functions) < 5 {
		t.Fatalf("a real guest mapped to only %d functions", len(m.Functions))
	}
	prev := 0
	found := false
	for _, f := range m.Functions {
		if f.Lua[0] <= prev {
			t.Errorf("%s starts at %d, inside the range before it (ends %d)",
				f.Name, f.Lua[0], prev)
		}
		prev = f.Lua[1]
		if f.Lua[1] > len(lines) {
			t.Fatalf("%s ends at line %d of a %d-line file", f.Name, f.Lua[1], len(lines))
		}
		raw := f.Name
		if f.Mangled != "" {
			raw = f.Mangled
		}
		if banner := lines[f.Lua[0]-1]; !strings.HasPrefix(banner, "-- "+raw+" ") {
			t.Errorf("%s claims line %d, which is %q", f.Name, f.Lua[0], banner)
		}
		if last := strings.TrimSpace(lines[f.Lua[1]-1]); last != "end" &&
			!strings.HasPrefix(last, "F[") {
			t.Errorf("%s ends at line %d, which is %q", f.Name, f.Lua[1], last)
		}
		if strings.Contains(f.Name, wantExport) {
			found = true
		}
	}
	if !found {
		var names []string
		for _, f := range m.Functions {
			names = append(names, f.Name)
		}
		sort.Strings(names)
		t.Errorf("no entry names %q; the map has %v", wantExport, names)
	}
}

// THE FIELD-NAME SET, checked against a document a real toolchain's names and
// DWARF filled in, so the optional fields are actually present.
func checkFieldSet(t *testing.T, body []byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for k := range doc {
		seen[k] = true
	}
	fns, _ := doc["functions"].([]any)
	for _, f := range fns {
		for k := range f.(map[string]any) {
			seen["functions[]."+k] = true
		}
	}
	var keys []string
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Every field the contract has. A guest with no Rust symbols carries no
	// `mangled`, so that one is checked where it is produced rather than here.
	for _, want := range []string{
		"fklua_map", "functions", "functions[].line", "functions[].lua",
		"functions[].name", "functions[].src", "functions[].wasm",
	} {
		if !seen[want] {
			t.Errorf("the map has no %s; its fields are %v", want, keys)
		}
	}
	for _, k := range keys {
		switch k {
		case "fklua_map", "functions", "module",
			"functions[].line", "functions[].lua", "functions[].mangled",
			"functions[].name", "functions[].src", "functions[].wasm":
		default:
			t.Errorf("the map carries an unpinned field %q", k)
		}
	}
}

// A GO GUEST, end to end: TinyGo compiles it, the map is built from what TinyGo
// emitted, and the ranges land on the packaged file's real lines.
//
// TinyGo ships DWARF at the flags this project's guests are built with, so the
// source positions here are not optional extras -- they are what the test
// checks, and a toolchain that stopped emitting them would say so here.
func TestAGoGuestGetsADebugMapWithSourceLines(t *testing.T) {
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	wasmPath := filepath.Join(t.TempDir(), "hello.wasm")
	if err := guest.Build(filepath.Join(root, "guest", "go"), "./examples/hello", wasmPath); err != nil {
		t.Fatalf("building the guest: %v", err)
	}
	m, body, lines := mapForGuest(t, wasmPath, "fk-map-go")
	checkGuestMap(t, m, lines, "main.")
	checkFieldSet(t, body)

	withSrc := 0
	for _, f := range m.Functions {
		if f.Src != "" && f.Line > 0 {
			withSrc++
		}
	}
	// Not every defined function has a subprogram with a low_pc -- TinyGo's
	// __wasm_call_ctors and its stack-pointer helper have none -- so this is a
	// majority rather than a total.
	if withSrc*2 <= len(m.Functions) {
		t.Errorf("only %d of %d entries carry a source position; TinyGo ships "+
			"DWARF at these flags and the join should find most of them",
			withSrc, len(m.Functions))
	}
	// The guest's own code, named from its own file.
	for _, f := range m.Functions {
		if strings.HasPrefix(f.Name, "main.") && f.Src != "" &&
			!strings.HasSuffix(f.Src, ".go") {
			t.Errorf("%s is attributed to %q", f.Name, f.Src)
		}
	}
}

// A RUST GUEST, on the same terms. Its release profile carries
// debug = "line-tables-only", which is what puts source positions in this map
// at all: without that key rustc emits no DWARF and the entries would be
// name-only.
//
// It also exercises the demangler against real v0 symbols, which is the half a
// table test cannot cover: the table has the symbols somebody looked at, and
// this has whatever the linker emitted today.
func TestARustGuestGetsADebugMapWithDemangledNames(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "cargo")
	wasmPath, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "hello", out)
	if err != nil {
		t.Fatalf("building the rust guest: %v", err)
	}
	m, body, lines := mapForGuest(t, wasmPath, "fk-map-rs")
	checkGuestMap(t, m, lines, "fk_on_")
	checkFieldSet(t, body)

	demangled, withSrc := 0, 0
	for _, f := range m.Functions {
		if f.Mangled != "" {
			demangled++
			if strings.HasPrefix(f.Name, "_R") {
				t.Errorf("%s was reported as demangled and still is a symbol", f.Name)
			}
			if !strings.Contains(f.Name, "::") {
				t.Errorf("%s does not read as a path", f.Name)
			}
		}
		if f.Src != "" && f.Line > 0 {
			withSrc++
		}
	}
	if demangled == 0 {
		t.Error("no v0 symbol was demangled; rustc mangles everything that is " +
			"not #[no_mangle], so either the scheme moved or the map lost the names")
	}
	if withSrc == 0 {
		t.Error("no entry carries a source position; the release profile's " +
			"debug key is what puts DWARF in a Rust guest, and it is gone")
	}
}
