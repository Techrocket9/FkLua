package main

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
)

// readMap parses a packaged map and returns it beside the module file its
// ranges are measured in.
func readMap(t *testing.T, dir, mapName, moduleName string) (mapDoc, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, mapName))
	if err != nil {
		t.Fatalf("reading the map: %v", err)
	}
	var m mapDoc
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the map is not JSON: %v\n%s", err, raw)
	}
	mod, err := os.ReadFile(filepath.Join(dir, moduleName))
	if err != nil {
		t.Fatalf("reading the module: %v", err)
	}
	return m, strings.Split(string(mod), "\n")
}

// mapDoc is a CONSUMER's view of the document, written out here rather than
// imported from internal/debugmap: this is the shape a tool outside the
// repository codes against, and a test that unmarshalled the producer's own
// struct could not tell a rename from a no-op.
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

// checkBrackets asserts what the whole map is for: the range really does
// contain that function's text in the PACKAGED file, wrapper and all.
func checkBrackets(t *testing.T, m mapDoc, lines []string) {
	t.Helper()
	if len(m.Functions) == 0 {
		t.Fatal("the map names no functions")
	}
	prev := 0
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
	}
}

// THE MAP IS EMITTED BY DEFAULT and its ranges land on the right lines of the
// packaged file. This is the +5 wrapper offset under test: it is applied where
// nothing else would notice it being wrong.
//
// Red proof: package with factorio.ChunkLineOffset set to 4 and every entry
// reports a banner line one short of its function.
func TestModWritesADebugMap(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", out}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(out, "a-mod_0.1.0")
	m, lines := readMap(t, dir, factorio.MapFile, factorio.GeneratedModuleFile)
	if m.Version != 1 {
		t.Errorf("fklua_map = %d, want 1", m.Version)
	}
	if m.Module != factorio.GeneratedModuleFile {
		t.Errorf("module = %q, want %q", m.Module, factorio.GeneratedModuleFile)
	}
	checkBrackets(t, m, lines)
}

// The data module gets its own map at its own offset, which is a different
// number because its wrapper carries an extra header line.
func TestModWritesADebugMapForTheDataModule(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{tinyGuest(t),
		"--data-module", dataGuest(t, "fk_data"),
		"--name", "a-mod", "--version", "0.1.0", "--author", "someone",
		"-o", out}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(out, "a-mod_0.1.0")
	m, lines := readMap(t, dir, factorio.DataMapFile, factorio.DataModuleFile)
	if m.Module != factorio.DataModuleFile {
		t.Errorf("module = %q, want %q", m.Module, factorio.DataModuleFile)
	}
	checkBrackets(t, m, lines)
	// The two offsets differ by one, so a data map built at the control offset
	// would bracket nothing. Proven by the bracketing above; named here because
	// it is the only reason there are two constants.
	if factorio.DataChunkLineOffset == factorio.ChunkLineOffset {
		t.Error("the two wrapper offsets are the same; one of them is wrong")
	}
}

// A DATA-STAGE-ONLY MOD is covered too: there is no control module for a map to
// be about, and the data one is still mapped.
func TestADataOnlyModStillGetsItsDataMap(t *testing.T) {
	out := t.TempDir()
	if err := runMod([]string{"--data-module", dataGuest(t, "fk_data"),
		"--name", "stand-in", "--version", "1.0.0", "--author", "someone",
		"-o", out}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(out, "stand-in_1.0.0")
	if _, err := os.Stat(filepath.Join(dir, factorio.MapFile)); err == nil {
		t.Errorf("a mod with no control module shipped %s", factorio.MapFile)
	}
	m, lines := readMap(t, dir, factorio.DataMapFile, factorio.DataModuleFile)
	checkBrackets(t, m, lines)
}

// --no-map subtracts one file and changes nothing else. A flag that also moved
// a byte of the mod would be a different packaging, not an opt-out.
func TestNoMapSubtractsOnlyTheMap(t *testing.T) {
	withMap := t.TempDir()
	without := t.TempDir()
	args := func(out string, extra ...string) []string {
		return append([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
			"--author", "someone", "-o", out}, extra...)
	}
	if err := runMod(args(withMap)); err != nil {
		t.Fatal(err)
	}
	if err := runMod(args(without, "--no-map")); err != nil {
		t.Fatal(err)
	}
	a := readTree(t, filepath.Join(withMap, "a-mod_0.1.0"))
	b := readTree(t, filepath.Join(without, "a-mod_0.1.0"))
	if _, ok := b[factorio.MapFile]; ok {
		t.Errorf("--no-map still wrote %s", factorio.MapFile)
	}
	if _, ok := a[factorio.MapFile]; !ok {
		t.Fatalf("no %s to leave out: %v", factorio.MapFile, a)
	}
	delete(a, factorio.MapFile)
	if len(a) != len(b) {
		t.Fatalf("--no-map changed the file list:\n  %v\n  %v", keysOf(a), keysOf(b))
	}
	for name, body := range a {
		if b[name] != body {
			t.Errorf("--no-map changed %s", name)
		}
	}
}

// A typo is a refusal, not a silent no-map. The unknown-flag arm covers it;
// this says so out loud, because a flag whose misspelling is ignored is a flag
// that quietly stops working.
func TestAMisspelledNoMapIsRefused(t *testing.T) {
	err := runMod([]string{tinyGuest(t), "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", t.TempDir(), "--nomap"})
	if err == nil {
		t.Fatal("--nomap was accepted")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("the message does not name the problem: %v", err)
	}
}

// The same guest packaged twice produces the same map, byte for byte. The map
// is the newest thing in the archive and the likeliest place for a map
// iteration order or a timestamp to get in.
func TestTheMapIsByteIdenticalAcrossPackagings(t *testing.T) {
	guest := tinyGuest(t)
	one, two := t.TempDir(), t.TempDir()
	for _, out := range []string{one, two} {
		if err := runMod([]string{guest, "--name", "a-mod", "--version", "0.1.0",
			"--author", "someone", "-o", out}); err != nil {
			t.Fatal(err)
		}
	}
	a := mustRead(t, filepath.Join(one, "a-mod_0.1.0", factorio.MapFile))
	b := mustRead(t, filepath.Join(two, "a-mod_0.1.0", factorio.MapFile))
	if a != b {
		t.Errorf("two packagings of one guest wrote different maps:\n%s\n%s", a, b)
	}
}

// TWO --zip RUNS PRODUCE THE SAME ARCHIVE, byte for byte.
//
// There was no test for this before the map existed, and the map is the reason
// to finally have one: it is the first generated file assembled from a walk
// over per-function data, and a reproducible archive is what a mod portal
// upload and a build cache both rest on.
func TestTwoZipRunsAreByteIdentical(t *testing.T) {
	guest := tinyGuest(t)
	one, two := t.TempDir(), t.TempDir()
	for _, out := range []string{one, two} {
		if err := runMod([]string{guest, "--zip", "--name", "a-mod",
			"--version", "0.1.0", "--author", "someone", "-o", out}); err != nil {
			t.Fatal(err)
		}
	}
	a := mustRead(t, filepath.Join(one, "a-mod_0.1.0.zip"))
	b := mustRead(t, filepath.Join(two, "a-mod_0.1.0.zip"))
	if a != b {
		t.Errorf("two --zip runs wrote different archives (%d and %d bytes)",
			len(a), len(b))
	}
}

// A directory and a zip carry the same map. They are two writers over one file
// list, and this is the assertion that keeps them one.
func TestTheZipCarriesTheSameMapAsTheDirectory(t *testing.T) {
	guest := tinyGuest(t)
	out := t.TempDir()
	if err := runMod([]string{guest, "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", out}); err != nil {
		t.Fatal(err)
	}
	zipOut := t.TempDir()
	if err := runMod([]string{guest, "--zip", "--name", "a-mod", "--version", "0.1.0",
		"--author", "someone", "-o", zipOut}); err != nil {
		t.Fatal(err)
	}
	want := mustRead(t, filepath.Join(out, "a-mod_0.1.0", factorio.MapFile))

	r, err := zip.OpenReader(filepath.Join(zipOut, "a-mod_0.1.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	name := "a-mod_0.1.0/" + factorio.MapFile
	for _, f := range r.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Error("the archived map differs from the one on disk")
		}
		return
	}
	t.Errorf("the archive has no %s", name)
}

// THE SCAFFOLDED RELEASE PROFILE CARRIES THE DEBUG KEY, and it is the same key
// this repository's own guest workspace carries.
//
// Without it rustc emits no DWARF at all for a wasm target, so a scaffolded
// Rust project's debug map would come out name-only while the repository's own
// guests carried source positions. That difference is invisible until somebody
// tries to read a map, which is the worst way to find it.
func TestTheScaffoldedRustProfileCarriesTheDebugKey(t *testing.T) {
	const key = `debug = "line-tables-only"`
	// Both arms of --data, because the profile is what a release build reads
	// and neither the data crate nor the control one may lose it.
	for _, data := range []bool{false, true} {
		scaffolded := rustWorkspaceCargo("a-mod", "", data)
		if !strings.Contains(scaffolded, key) {
			t.Errorf("the scaffolded workspace manifest (data=%v) does not carry "+
				"%s:\n%s", data, key, scaffolded)
		}
	}
	scaffolded := rustWorkspaceCargo("a-mod", "", false)
	committed, err := os.ReadFile(filepath.Join("..", "..", "guest", "rust", "Cargo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(committed), key) {
		t.Errorf("guest/rust/Cargo.toml does not carry %s, so this repository's "+
			"own guests and a scaffolded one would disagree", key)
	}
	// The rest of the profile is a requirement rather than a preference, and a
	// debug key added by deleting one of them would pass the check above.
	for _, want := range []string{`opt-level = "s"`, "lto = true", `panic = "abort"`} {
		if !strings.Contains(scaffolded, want) {
			t.Errorf("the scaffolded release profile lost %s", want)
		}
	}
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out[e.Name()] = mustRead(t, filepath.Join(dir, e.Name()))
	}
	return out
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
