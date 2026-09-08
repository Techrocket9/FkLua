package guest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// A DATA-STAGE STRING IS BYTES, AND BOTH GUEST LIBRARIES HAND BACK THE SAME
// ONES.
//
// The control ABI settled this once already, for the generated bindings:
// `TestABinaryStringCrossesAGeneratedEventReaderByteExact` and the `LuaStr`
// type it pins. The data ABI had the same asymmetry and nothing had asked it.
// Go's fkdata builds its string from the raw bytes, because a Go string IS a
// byte string; Rust's decoded with `String::from_utf8_lossy`, which replaces
// every byte outside UTF-8 with U+FFFD and CHANGES THE LENGTH while it does it.
// So for any value the engine carries through unaltered the two guests saw
// different strings of different lengths, and every string in this repository's
// own data-stage corpus is a prototype name -- ASCII by engine constraint -- so
// nothing here could see it. It was found by a library outside this repo, which
// is the same shape and the same discovery route as the control-ABI instance.
//
// So this drives BOTH `databytes` guests -- one host stand-in, one script, one
// set of expectations -- with three byte strings through two different read
// paths and two different write paths, and asserts at the BYTES:
//
//   - a STORED STARTUP SETTING, read through env(3). That is where a data guest
//     meets arbitrary bytes for real: measured on 2.0.77, invalid UTF-8 and
//     control characters survive mod-settings.dat and arrive at the stage
//     unchanged. A NUL truncates the stored value inside the engine, which is
//     the engine's behaviour and not this ABI's -- the NUL below is driving
//     fk_data.lua, which is the ABI, and a Lua string is counted rather than
//     terminated.
//   - a STRING FIELD of a prototype, read through `get`. A different decode path
//     over the same wire, and the shape another mod's data arrives in.
//   - the PROTOTYPE NAMES at a type, read through `keys`. A third decode path,
//     and the only one that hands back a whole SET of names the engine wrote:
//     the fixture's other two enumerate nothing, so a lossy `keys` stayed
//     invisible until this arrived.
//   - the setting's bytes straight back OUT through `set`, and the field's bytes
//     back out through `extend`, both printed by the HOST from what landed in
//     its own `data.raw`, so the outbound half is pinned too.
//
// The three shapes are all 256 byte values, a short string carrying NUL (in the
// middle on one path and at both ends on the other), and the empty string.
// Everything is printed as `<len>:<hex>`, because the lossy reader got the
// LENGTH wrong as well as the contents and a check asserting only one of them
// could have passed.
//
// Both arms are checked against ONE derived expectation, so the transcripts are
// required to be identical as well as right. That is the property
// `TestBothDataGuestLibrariesMakeTheSameCalls` exists for one level up: fkdata
// is hand-written in both languages, so census.json cannot see either of them.
func TestADataStageStringCrossesFkdataByteExact(t *testing.T) {
	goDir := buildDataBytesMod(t, "go")
	rsDir := buildDataBytesMod(t, "rust")
	goOut := runDataBytesMod(t, goDir)
	rsOut := runDataBytesMod(t, rsDir)

	// ONE EXPECTATION, BOTH ARMS, which is how the control ABI's byte-exactness
	// test states the mirror property too: a separate go-against-rust
	// comparison would be implied by these two and could not fail on its own,
	// and this repository counts a gate that cannot fail as worse than one
	// nobody added. Both arms report rather than abort, so a broken one does not
	// hide what the other did.
	want := strings.Join(dataBytesExpectation(), "\n")
	checkDataBytes(t, "go", goOut, want)
	checkDataBytes(t, "rust", rsOut, want)
}

// checkDataBytes compares one transcript against another, reports the FIRST
// line they differ at, and says what went wrong rather than printing two
// 600-character strings side by side.
func checkDataBytes(t *testing.T, lang, out, want string) {
	t.Helper()
	if out == want {
		return
	}
	gl := strings.Split(out, "\n")
	wl := strings.Split(want, "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		g, w := "", ""
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g == w {
			continue
		}
		// U+FFFD is efbfbd in the hex. Rust's from_utf8_lossy substitutes per
		// maximal subpart and Go's strings.ToValidUTF8 collapses a whole run
		// into one, so the two lossy readings are not even the same wrong
		// answer: 512 bytes with 128 of them against 131 bytes with one.
		lossy := ""
		if strings.Contains(g, "efbfbd") {
			lossy = " -- and there is a U+FFFD in it, which is a lossy reader"
		}
		t.Errorf("%s line %d is not byte-exact (got %d chars, want %d)%s:\n"+
			"  got  %s\n  want %s", lang, i+1, len(g), len(w), lossy, g, w)
		return
	}
	t.Errorf("%s: the transcripts differ in length: %d lines against %d",
		lang, len(gl), len(wl))
}

// The three byte strings, built here rather than written out, because 512
// characters of hex nobody can check is not an expectation.
//
// The SETTING and the FIELD carry DIFFERENT bytes on purpose: two read paths
// wired to one source would agree however either of them was broken.
func dataBytesValues() (setting, field map[string][]byte) {
	asc := make([]byte, 256)
	desc := make([]byte, 256)
	for i := 0; i < 256; i++ {
		asc[i] = byte(i)
		desc[i] = byte(255 - i)
	}
	setting = map[string][]byte{
		"all":   asc,
		"empty": {},
		// A NUL in the middle, which is where a C string would end.
		"nul": {'a', 0, 'b'},
	}
	field = map[string][]byte{
		"all":   desc,
		"empty": {},
		// NUL at BOTH ends as well, which is where a trimming reader would eat
		// one.
		"nul": {0, 'z', 0},
	}
	return setting, field
}

var dataBytesProbes = []string{"all", "empty", "nul"}

// The prototype TYPE the guests enumerate, and the NAMES under it, in the byte
// order fk_data.lua sorts them into (verified under bin/lua52f: Lua's string
// order is byte order for these under LC_ALL=C and under a UTF-8 locale alike).
//
// A prototype name is a key in a global namespace any mod can write, so it is
// bytes like every other string here. The names are what make `keys` testable:
// with only ASCII names a lossy reading of them is the identity, which is
// exactly how a lossy `keys` survived the first version of this fixture.
const dataBytesKeyedType = "fkb-keyed"

func dataBytesKeyNames() [][]byte {
	return [][]byte{
		[]byte("k-plain"),
		// A lone continuation byte, with nothing in front of it.
		{'k', '-', 0x80},
		// And two bytes no UTF-8 sequence ever starts with.
		{'k', '-', 0xff, 0xfe},
	}
}

func hexOf(b []byte) string {
	return fmt.Sprintf("%d:%x", len(b), b)
}

// dataBytesExpectation is the whole transcript, derived from the same values the
// Lua stand-in is built from.
func dataBytesExpectation() []string {
	setting, field := dataBytesValues()
	var out []string
	for _, p := range dataBytesProbes {
		out = append(out,
			"LOG databytes: setting "+p+" "+hexOf(setting[p]),
			"LOG databytes: field "+p+" "+hexOf(field[p]))
	}
	// The sorted prototype NAMES at a type, out of `keys`.
	for _, k := range dataBytesKeyNames() {
		out = append(out, "LOG databytes: key "+hexOf(k))
	}
	// What `set` put back: the SETTING's bytes.
	for _, p := range dataBytesProbes {
		out = append(out, "ECHO "+p+" "+hexOf(setting[p]))
	}
	// What `extend` put back: the FIELD's bytes.
	for _, p := range dataBytesProbes {
		out = append(out, "EXTEND "+p+" "+hexOf(field[p]))
	}
	// And the source the guest read is unchanged, because carrying a value is
	// not supposed to touch it.
	for _, p := range dataBytesProbes {
		out = append(out, "SOURCE "+p+" "+hexOf(field[p]))
	}
	return out
}

// buildDataBytesMod compiles the byte-exactness data guest in one language and
// packages it, returning the mod directory.
func buildDataBytesMod(t *testing.T, lang string) string {
	t.Helper()
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root := repoRoot(t)
	tmp := t.TempDir()

	var wasmPath string
	switch lang {
	case "go":
		wasmPath = filepath.Join(tmp, "databytes.wasm")
		if err := guest.Build(filepath.Join(root, "guest", "go"),
			"./examples/databytes", wasmPath); err != nil {
			t.Fatalf("building the Go data guest: %v", err)
		}
	case "rust":
		if ok, why := guest.RustAvailable(); !ok {
			t.Skipf("skipping the Rust arm: %s", why)
		}
		p, err := guest.BuildRust(filepath.Join(root, "guest", "rust"), "databytes",
			filepath.Join(tmp, "rust-target"))
		if err != nil {
			t.Fatalf("building the Rust data guest: %v", err)
		}
		wasmPath = p
	default:
		t.Fatalf("unknown language %q", lang)
	}

	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := wasm.Decode(raw)
	if err != nil {
		t.Fatalf("decoding the data guest: %v", err)
	}
	var imports, exports []string
	for _, im := range mod.Imports {
		imports = append(imports, im.Module+"."+im.Name)
	}
	for _, e := range mod.Exports {
		exports = append(exports, e.Name)
	}
	if err := factorio.CheckDataModule(imports, exports); err != nil {
		t.Fatalf("the %s data guest is not a data module: %v", lang, err)
	}

	im, err := ir.BuildModule(mod)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Errorf("function %q did not compile: %v", f.Name, f.Unsupported)
		}
	}
	src, err := luagen.EmitModuleWith(im, luagen.Options{
		Persist: luagen.PersistNone, GC: luagen.GCLeaking,
		Roots: factorio.StageExportNames(),
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	pkg := &factorio.Package{
		Info: factorio.Info{
			Name: "fkb-example", Version: "0.1.0", Title: "FkLua data stage bytes",
			Author: "FkLua", FactorioVersion: factorio.DefaultFactorioVersion,
		},
		Chunk:       "return { exports = {} }",
		DataChunk:   src,
		DataExports: exports,
	}
	dir, err := pkg.WriteDir(tmp)
	if err != nil {
		t.Fatalf("packaging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data.lua")); err != nil {
		t.Fatalf("the packaged mod has no data.lua: %v", err)
	}
	return dir
}

func runDataBytesMod(t *testing.T, modDir string) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	out, err := h.RunString(dataBytesHarness(modDir))
	if err != nil {
		t.Fatalf("running the data stage: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// dataBytesHarness is the engine-shaped stand-in, built from the same values the
// expectation is, so the two cannot drift.
//
// The byte strings reach Lua as HEX and are decoded there, rather than as a
// string.char call per value: a 256-argument call is "function or expression
// too complex" in a real Lua 5.2 parser, which is a fact about the fixture and
// not about the ABI. Lua strings are counted rather than NUL-terminated, so all
// three are ordinary values once decoded and it is the ABI's job to keep them
// that way.
func dataBytesHarness(modDir string) string {
	setting, field := dataBytesValues()
	luaBytes := func(b []byte) string { return fmt.Sprintf("fromhex(%q)", fmt.Sprintf("%x", b)) }
	var startup, fields []string
	for _, p := range dataBytesProbes {
		startup = append(startup,
			fmt.Sprintf("  [%q] = { value = %s },", "fkb-"+p, luaBytes(setting[p])))
		fields = append(fields, fmt.Sprintf("    [%q] = %s,", p, luaBytes(field[p])))
	}
	// Written in the order the expectation lists them, which is also the sorted
	// order: a fixture that fed them in sorted and asserted them in sorted would
	// be asserting its own input order, so the shim's sort is what has to put
	// them there. Reversed on the way in for exactly that reason.
	var keyed []string
	names := dataBytesKeyNames()
	for i := len(names) - 1; i >= 0; i-- {
		keyed = append(keyed, fmt.Sprintf("  { type = %q, name = %s },",
			dataBytesKeyedType, luaBytes(names[i])))
	}

	return fmt.Sprintf(`package.path = %q
function log(s) print("LOG " .. s) end

mods = { base = "2.0.77", ["fkb-example"] = "0.1.0" }
feature_flags = {}
defines = { prototypes = {} }

-- <len>:<hex>, the same rendering the guest logs, so a value the guest printed
-- and a value the host read back are comparable at a glance.
local function hexline(s)
  return #s .. ":" .. (s:gsub(".", function(c) return string.format("%%02x", c:byte()) end))
end

-- The inverse, for the fixture values below.
local function fromhex(h)
  local out = {}
  for i = 1, #h, 2 do out[#out+1] = string.char(tonumber(h:sub(i, i + 1), 16)) end
  return table.concat(out)
end

data = { raw = {} }
function data:extend(list)
  if type(list) ~= "table" then error("data:extend takes a table", 0) end
  for i, p in ipairs(list) do
    if type(p) ~= "table" then error("data:extend entry " .. i .. " is not a table", 0) end
    if type(p.type) ~= "string" then error("data:extend entry " .. i .. " has no type", 0) end
    if type(p.name) ~= "string" then error("data:extend entry " .. i .. " has no name", 0) end
    data.raw[p.type] = data.raw[p.type] or {}
    data.raw[p.type][p.name] = p
  end
end

-- The prototype the guest reads its string fields out of.
data:extend{
  { type = "fkb-probe", name = "fkb-source",
%s
  },
}

-- The prototype TYPE the guest ENUMERATES. Its names hold bytes outside UTF-8,
-- because a prototype name is a key in a table any mod can write, and the keys
-- primitive has to sort them and hand each one back byte-exact.
data:extend{
%s
}

-- A STORED SETTING VALUE, which is the one string at a data stage a player can
-- put arbitrary bytes into: measured on Factorio 2.0.77, invalid UTF-8 and
-- control characters survive mod-settings.dat and arrive here unchanged. A NUL
-- truncates the stored value inside the engine, which is the engine's own
-- behaviour and not this ABI's, so the NUL below is a statement about
-- fk_data.lua -- which is what this fixture drives -- and not about the engine.
settings = { startup = {
%s
} }

require("data")

local src = data.raw["fkb-probe"]["fkb-source"]
local probes = { %s }
for _, k in ipairs(probes) do
  print("ECHO " .. k .. " " .. hexline(src["echo-" .. k]))
end
local echo = data.raw["fkb-echo"]["fkb-echo-out"]
for _, k in ipairs(probes) do
  print("EXTEND " .. k .. " " .. hexline(echo[k]))
end
for _, k in ipairs(probes) do
  print("SOURCE " .. k .. " " .. hexline(src[k]))
end
`, filepath.Join(modDir, "?.lua"),
		strings.Join(fields, "\n"),
		strings.Join(keyed, "\n"),
		strings.Join(startup, "\n"),
		`"`+strings.Join(dataBytesProbes, `", "`)+`"`)
}
