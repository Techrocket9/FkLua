package debugmap

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/wasm"
)

func sampleSpans() []luagen.FuncSpan {
	return []luagen.FuncSpan{
		{Name: "memcpy", Index: 2, Start: 10, End: 20},
		{Name: "_RNvNtCs1AJuqHJ3rjd_4fkgc7collect9mark_step", Index: 3, Start: 22, End: 40},
		{Name: "func[4]", Index: 4, Start: 42, End: 44},
	}
}

// The wrapper offset is applied to BOTH ends, and the wasm index is carried
// through untouched. Written out rather than derived, because a map whose
// arithmetic is checked by the same arithmetic that produced it checks nothing.
func TestTheWrapperOffsetMovesEveryRange(t *testing.T) {
	m := Build("fk_module.lua", sampleSpans(), 5, nil)
	if m.Module != "fk_module.lua" {
		t.Errorf("module = %q, want the LUA file the ranges are in", m.Module)
	}
	if m.Version != 1 {
		t.Errorf("fklua_map = %d, want 1", m.Version)
	}
	want := [][2]int{{15, 25}, {27, 45}, {47, 49}}
	for i, f := range m.Functions {
		if f.Lua != want[i] {
			t.Errorf("entry %d has range %v, want %v", i, f.Lua, want[i])
		}
	}
	if got := []uint32{m.Functions[0].Wasm, m.Functions[1].Wasm, m.Functions[2].Wasm}; got[0] != 2 || got[1] != 3 || got[2] != 4 {
		t.Errorf("wasm indices = %v, want 2 3 4", got)
	}
}

// Ascending by first line, which is what makes a binary search legal.
func TestEntriesAreSortedByTheirFirstLine(t *testing.T) {
	spans := sampleSpans()
	spans[0], spans[2] = spans[2], spans[0]
	m := Build("fk_module.lua", spans, 0, nil)
	prev := 0
	for _, f := range m.Functions {
		if f.Lua[0] <= prev {
			t.Fatalf("entry at %d follows one ending at %d: %v", f.Lua[0], prev, m.Functions)
		}
		prev = f.Lua[1]
	}
}

// A Rust symbol is demangled and keeps its raw form beside it; everything else
// is left alone and carries no second field.
func TestOnlyADemangledEntryCarriesItsSymbol(t *testing.T) {
	m := Build("fk_module.lua", sampleSpans(), 0, nil)
	if m.Functions[0].Name != "memcpy" || m.Functions[0].Mangled != "" {
		t.Errorf("a plain name was rewritten: %+v", m.Functions[0])
	}
	if got := m.Functions[1].Name; got != "fkgc::collect::mark_step" {
		t.Errorf("name = %q, want the demangled path", got)
	}
	if !strings.HasPrefix(m.Functions[1].Mangled, "_R") {
		t.Errorf("the raw symbol is missing: %+v", m.Functions[1])
	}
	if m.Functions[2].Name != "func[4]" {
		t.Errorf("the synthetic name was rewritten: %+v", m.Functions[2])
	}
}

// THE FIELD-NAME SET IS THE CONTRACT with whatever reads this file, and a
// consumer outside this repository cannot be recompiled when a name moves. A
// rename that a typed unmarshal would swallow fails here; so does a new field,
// which is the point -- adding one is fine and it moves the contract on
// purpose rather than by accident.
func TestTheMapFieldSetIsPinned(t *testing.T) {
	m := Build("fk_module.lua", sampleSpans(), 5, nil)
	// The DWARF fields are absent from every entry above, so one is filled in
	// by hand: an optional field still belongs to the contract.
	m.Functions[0].Src, m.Functions[0].Line = "main.go", 87
	b, err := m.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the map is not JSON: %v\n%s", err, b)
	}
	var keys []string
	for k := range doc {
		keys = append(keys, k)
	}
	fns, _ := doc["functions"].([]any)
	if len(fns) != 3 {
		t.Fatalf("functions is %T with %d entries", doc["functions"], len(fns))
	}
	seen := map[string]bool{}
	for _, f := range fns {
		for k := range f.(map[string]any) {
			seen["functions[]."+k] = true
		}
	}
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{
		"fklua_map", "functions",
		"functions[].line", "functions[].lua", "functions[].mangled",
		"functions[].name", "functions[].src", "functions[].wasm",
		"module",
	}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Errorf("the document's fields are\n  %v\nand the contract is\n  %v", keys, want)
	}
}

// Two-space indent and a trailing newline, like every other JSON this compiler
// writes -- and byte-identical for identical input, which is what a
// reproducible --zip archive rests on.
func TestTheDocumentIsFormattedAndStable(t *testing.T) {
	a, err := Build("fk_module.lua", sampleSpans(), 5, nil).JSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build("fk_module.lua", sampleSpans(), 5, nil).JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("two renderings of the same map differ")
	}
	if !strings.HasSuffix(string(a), "}\n") {
		t.Errorf("the document does not end in a newline:\n%q", string(a[len(a)-8:]))
	}
	if !strings.Contains(string(a), "\n    {\n      \"lua\": [") {
		t.Errorf("the document is not indented two spaces:\n%s", a)
	}
	// An empty function list is [] and never null: a consumer that iterates it
	// must not have to special-case a guest with nothing in it.
	empty, err := Build("fk_module.lua", nil, 5, nil).JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"functions": []`) {
		t.Errorf("an empty map renders as\n%s", empty)
	}
}

// A guest with no debug information gets the name-only baseline, which is the
// guaranteed form. Nothing about this is an error.
func TestAGuestWithoutDwarfStillGetsAMap(t *testing.T) {
	m, err := wasm.DecodeWAT(`(module (func) (func (nop)))`)
	if err != nil {
		t.Fatal(err)
	}
	got := Build("fk_module.lua", sampleSpans(), 5, m)
	if len(got.Functions) != 3 {
		t.Fatalf("%d entries, want 3", len(got.Functions))
	}
	if n := got.WithSource(); n != 0 {
		t.Errorf("%d entries carry a source position from a module with no DWARF", n)
	}
	for _, f := range got.Functions {
		if f.Src != "" || f.Line != 0 {
			t.Errorf("%+v was given a source position", f)
		}
	}
}

// DWARF THAT CANNOT BE READ IS NOT AN ERROR. A guest built by a toolchain this
// does not understand, or one whose sections were mangled by a post-processor,
// must still package -- with fewer fields filled in and nothing said about it.
func TestUnreadableDwarfIsNotFatal(t *testing.T) {
	m, err := wasm.DecodeWAT(`(module (func) (func (nop)))`)
	if err != nil {
		t.Fatal(err)
	}
	m.Custom = []wasm.CustomSection{
		{Name: ".debug_info", Payload: []byte("not dwarf, not even close")},
		{Name: ".debug_abbrev", Payload: []byte{0xff, 0xff, 0xff}},
		{Name: ".debug_line", Payload: []byte{0x01}},
		{Name: ".debug_str", Payload: nil},
	}
	m.CodeSpans = []wasm.CodeSpan{{Lo: 2, Hi: 4}, {Lo: 5, Hi: 8}}
	got := Build("fk_module.lua", sampleSpans(), 5, m)
	if len(got.Functions) != 3 {
		t.Fatalf("%d entries, want 3", len(got.Functions))
	}
	if n := got.WithSource(); n != 0 {
		t.Errorf("%d entries were given a source position out of nonsense", n)
	}
}

// The spans and the module are joined by wasm function index, so a module whose
// span list does not line up with its function list contributes nothing rather
// than contributing the wrong thing.
func TestAMismatchedModuleContributesNothing(t *testing.T) {
	m, err := wasm.DecodeWAT(`(module (func) (func (nop)))`)
	if err != nil {
		t.Fatal(err)
	}
	m.CodeSpans = []wasm.CodeSpan{{Lo: 2, Hi: 4}}
	if got := sourceLines(m); got != nil {
		t.Errorf("a module with %d spans for %d functions resolved %d positions",
			len(m.CodeSpans), len(m.Funcs), len(got))
	}
	if got := sourceLines(nil); got != nil {
		t.Errorf("a nil module resolved %d positions", len(got))
	}
}

// comp_dir cleanup, which is what turns a build-machine path into one an author
// recognises -- and which must never fire on a path that merely looks similar.
func TestCompDirRelativePaths(t *testing.T) {
	for _, tc := range []struct{ dir, path, want string }{
		{"/rustc/8bab26f", "/rustc/8bab26f/library/core/src/option.rs", "library/core/src/option.rs"},
		{"/work/guest/rust/", "/work/guest/rust/mymod/src/lib.rs", "mymod/src/lib.rs"},
		{"/work/guest", "/work/guest-other/src/lib.rs", "/work/guest-other/src/lib.rs"},
		{"", "/anywhere/main.go", "/anywhere/main.go"},
		{"/work", "/work", "/work"},
		{"/work", "", ""},
	} {
		c := &cuState{compDir: tc.dir}
		if got := c.clean(tc.path); got != tc.want {
			t.Errorf("comp_dir %q, path %q: got %q, want %q", tc.dir, tc.path, got, tc.want)
		}
	}
}
