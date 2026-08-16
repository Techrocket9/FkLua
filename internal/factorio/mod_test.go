package factorio

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	luart "github.com/Techrocket9/fklua/runtime"
)

func testPackage() *Package {
	return &Package{
		Info: Info{
			Name: "fk-hello", Version: "0.1.0", Title: "Hello",
			Author: "FkLua", FactorioVersion: DefaultFactorioVersion,
		},
		Chunk:   "local F = {}\nreturn { funcs = F, exports = {} }\n",
		Exports: []string{"_initialize", "fk_on_tick"},
	}
}

func TestPackageWritesALoadableShape(t *testing.T) {
	dir, err := testPackage().WriteDir(t.TempDir())
	if err != nil {
		t.Fatalf("WriteDir: %v", err)
	}
	if filepath.Base(dir) != "fk-hello_0.1.0" {
		t.Errorf("directory is %q; Factorio identifies a mod by <name>_<version>",
			filepath.Base(dir))
	}
	for _, name := range []string{"info.json", "control.lua", GeneratedModuleFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "info.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("info.json is not valid JSON: %v\n%s", err, raw)
	}
	// Factorio refuses to load a mod missing any of these.
	for _, k := range []string{"name", "version", "title", "author", "factorio_version"} {
		if s, _ := got[k].(string); s == "" {
			t.Errorf("info.json has no %q: %s", k, raw)
		}
	}
	// Asserted as a RELATIONSHIP and not as a literal, because the literal is
	// what drifts: `api = "2.1.14"` with `factorio_version = "2.0"` is two
	// answers to one question, which is the shape that cost this repo `gc` and
	// then `api`. Whatever the pin becomes, the declared series is its
	// major.minor -- and it is a series, so it carries no patch component.
	series, _ := got["factorio_version"].(string)
	if want := DefaultFactorioVersion; series != want {
		t.Errorf("factorio_version is %q, want %q -- the packaged series must be "+
			"the one derived from the API pin, not a second constant", series, want)
	}
	if !strings.HasPrefix(DefaultAPIVersion, series+".") {
		t.Errorf("factorio_version %q is not the major.minor of the API pin %q; "+
			"a mod whose bindings and whose info.json name different series is "+
			"refused by the loader at game start", series, DefaultAPIVersion)
	}
	if strings.Count(series, ".") != 1 {
		t.Errorf("factorio_version is %q; info.json takes a major.minor series, "+
			"and naming a patch release makes the mod unloadable", series)
	}
}

// Repackaging after a guest change is the common case. Leaving stale files
// behind produces a mod that half-updates, which is worse than either outcome.
func TestWriteDirReplacesAnExistingMod(t *testing.T) {
	parent := t.TempDir()
	dir, err := testPackage().WriteDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.lua")
	if err := os.WriteFile(stale, []byte("-- left over\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := testPackage().WriteDir(parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a stale file survived repackaging")
	}
}

// A zip whose files sit at the root is not a mod: Factorio wants them under a
// "<name>_<version>/" directory inside the archive.
func TestZipCarriesTheModDirectory(t *testing.T) {
	// Into a directory that does not exist yet: WriteDir creates its parent as
	// a side effect, so -o must behave the same in both modes.
	path, err := testPackage().WriteZip(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("WriteZip: %v", err)
	}
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("opening the zip: %v", err)
	}
	defer r.Close()

	want := map[string]bool{
		"fk-hello_0.1.0/info.json":              false,
		"fk-hello_0.1.0/control.lua":            false,
		"fk-hello_0.1.0/" + GeneratedModuleFile: false,
		"fk-hello_0.1.0/" + ABIFile:             false,
		"fk-hello_0.1.0/" + APIFile:             false,
	}
	for _, f := range r.File {
		if _, ok := want[f.Name]; !ok {
			t.Errorf("unexpected archive entry %q", f.Name)
			continue
		}
		want[f.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("archive is missing %q", name)
		}
	}
}

func TestValidateRejectsWhatFactorioWould(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutas func(*Info)
		want  string
	}{
		{"empty name", func(i *Info) { i.Name = "" }, "mod name"},
		{"name with a slash", func(i *Info) { i.Name = "a/b" }, "mod name"},
		{"two-part version", func(i *Info) { i.Version = "0.1" }, "mod version"},
		{"non-numeric version", func(i *Info) { i.Version = "v0.1.0" }, "mod version"},
		{"no title", func(i *Info) { i.Title = "" }, "title"},
		{"no author", func(i *Info) { i.Author = "" }, "author"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPackage()
			tc.mutas(&p.Info)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q should mention %q", err.Error(), tc.want)
			}
		})
	}
}

// The generated chunk reads its imports from `...` and ends in `return {...}`.
// Wrapping it in a vararg function is what makes both survive `require`, which
// hands a chunk no arguments of its own.
func TestGeneratedModuleIsAFactory(t *testing.T) {
	files, err := testPackage().Files()
	if err != nil {
		t.Fatal(err)
	}
	mod := files[GeneratedModuleFile]
	if !strings.Contains(mod, "return function(...)") {
		t.Errorf("%s is not a vararg factory:\n%s", GeneratedModuleFile, mod)
	}
	if !strings.HasSuffix(mod, "end\n") {
		t.Errorf("%s does not close its wrapper:\n%s", GeneratedModuleFile, mod)
	}
}

// control.lua and the packager have to agree on the generated file's name, and
// nothing else enforces it: a rename would produce a mod that fails to load
// with "module 'fk_module' not found" at game start.
func TestGlueRequiresTheFileThePackagerWrites(t *testing.T) {
	want := `require("` + strings.TrimSuffix(GeneratedModuleFile, ".lua") + `")`
	if !strings.Contains(luart.ModGlue(), want) {
		t.Errorf("control.lua does not contain %s", want)
	}
}

// Hooks is what `fklua mod` reports as wired. An entry here that control.lua
// does not actually register would be reported as connected and then silently
// never called, which is the worst of both.
func TestEveryReportedHookIsActuallyRegistered(t *testing.T) {
	glue := luart.ModGlue()
	for _, h := range Hooks {
		if !strings.Contains(glue, "E."+h.Export) {
			t.Errorf("Hooks lists %q but control.lua never references it", h.Export)
		}
	}
}

// And the other direction, which is the one that actually drifted: control.lua
// called an export Hooks did not list, for two milestones.
//
// Hooks is not just a report. `fklua mod` passes it to the emitter as the
// reachability ROOT SET, so an entry point missing from it makes everything
// only that entry point reaches look dead -- and a NaN diagnostic inside a
// guest's event handlers was silently dropped on the one path that packages a
// mod. Inert() reads it too, so a guest wired entirely through fk.subscribe was
// told it would never run.
func TestEveryExportControlLuaCallsIsListedInHooks(t *testing.T) {
	listed := map[string]bool{}
	for _, h := range Hooks {
		listed[h.Export] = true
	}
	// `E` is control.lua's name for the guest's export table, so E.<name> is
	// every export it can possibly call.
	re := regexp.MustCompile(`\bE\.([A-Za-z_][A-Za-z0-9_]*)`)
	for _, m := range re.FindAllStringSubmatch(luart.ModGlue(), -1) {
		if !listed[m[1]] {
			t.Errorf("control.lua calls the guest export %q, but Hooks does not "+
				"list it -- so `fklua mod` treats it as unreachable when it "+
				"chooses diagnostic roots, and never reports it as wired", m[1])
		}
	}
}

// CollectorSurface is a precondition and not a report: `--gc=collected` is
// refused for a module that does not export all of it. So it has to be exactly
// the set control.lua tests before it wires the pacing up, in both directions.
// Derived one way (a prefix over Hooks) and asserted the other (what the glue
// actually names), because a prefix rule is a guess about the future and this
// is the thing that catches the guess being wrong.
func TestTheCollectorSurfaceIsWhatControlLuaBinds(t *testing.T) {
	have := map[string]bool{}
	for _, n := range CollectorSurface() {
		have[n] = true
	}
	if len(have) == 0 {
		t.Fatal("CollectorSurface is empty, so --gc=collected would be refused " +
			"for every guest including a real collected one")
	}
	// Every fk_gc_ export control.lua binds, which is the definitive list: it
	// is what the pacing path dereferences.
	re := regexp.MustCompile(`\bE\.(fk_gc_[A-Za-z0-9_]*)`)
	binds := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(luart.ModGlue(), -1) {
		binds[m[1]] = true
	}
	for n := range binds {
		if !have[n] {
			t.Errorf("control.lua binds %q but CollectorSurface does not "+
				"require it, so --gc=collected would be accepted for a guest "+
				"whose pacing path is missing a function", n)
		}
	}
	for n := range have {
		if !binds[n] {
			t.Errorf("CollectorSurface requires %q but control.lua never binds "+
				"it, so --gc=collected is refused over an export nothing reads", n)
		}
	}
}

func TestInertReportsAGuestThatWillNeverRun(t *testing.T) {
	p := testPackage()
	p.Exports = []string{"_initialize"}
	if !p.Inert() {
		t.Error("a guest exporting only the TinyGo entry point is inert: " +
			"it loads, initialises and is never called again")
	}
	p.Exports = append(p.Exports, "fk_on_init")
	if p.Inert() {
		t.Error("a guest exporting fk_on_init is not inert")
	}

	// The shape M7 actually intends: no fk_on_tick, everything through
	// fk.subscribe and fk_on_event. It is not inert, and saying so sent an
	// author looking for a bug in a mod that was wired correctly.
	p.Exports = []string{"_initialize", "fk_on_event"}
	if p.Inert() {
		t.Error("a guest that receives events through fk_on_event is not inert")
	}
}

// dataStage writes a mod's declarative half: the shape every non-trivial mod
// has and this packager could not carry.
func dataStage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"data.lua":                 "require(\"prototypes.belt\")\n",
		"prototypes/belt.lua":      "data:extend{}\n",
		"locale/en/strings.cfg":    "[mod-name]\n",
		"graphics/icons/thing.png": "\x89PNG\r\n\x1a\n\x00binary\xff",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A DATA STAGE IS NOT AN EXOTIC REQUIREMENT.
//
// Files() returned exactly five generated entries and WriteDir removes the
// target first, so data.lua, prototypes/, graphics/ and locale/ -- declarative,
// no runtime, nothing for a guest to compile -- had nowhere to go. --zip was
// therefore unusable for any real mod, and the first downstream consumer copied
// its data stage over the output after packaging.
func TestIncludedFilesAreCarriedIntoTheMod(t *testing.T) {
	p := testPackage()
	if err := p.Include(dataStage(t)); err != nil {
		t.Fatal(err)
	}
	dir, err := p.WriteDir(t.TempDir())
	if err != nil {
		t.Fatalf("WriteDir: %v", err)
	}
	// Nested paths keep their shape: Factorio's require("prototypes.belt")
	// resolves against the mod root, so a flattened tree is a broken mod.
	for _, name := range []string{"data.lua", "prototypes/belt.lua",
		"locale/en/strings.cfg", "graphics/icons/thing.png"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	// Bytes, not text. Graphics are the majority of a real mod's data stage.
	got, err := os.ReadFile(filepath.Join(dir, "graphics", "icons", "thing.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x89PNG\r\n\x1a\n\x00binary\xff" {
		t.Errorf("the PNG did not survive the copy: %q", got)
	}
	// And the generated half is still all there.
	if _, err := os.Stat(filepath.Join(dir, "control.lua")); err != nil {
		t.Errorf("including files displaced the generated ones: %v", err)
	}
}

// --zip is the half that was unusable, so it gets its own assertion rather than
// trusting that a shared Files() means a shared outcome.
func TestAZipCarriesTheIncludedFilesToo(t *testing.T) {
	p := testPackage()
	if err := p.Include(dataStage(t)); err != nil {
		t.Fatal(err)
	}
	path, err := p.WriteZip(t.TempDir())
	if err != nil {
		t.Fatalf("WriteZip: %v", err)
	}
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	have := map[string]bool{}
	for _, f := range r.File {
		have[f.Name] = true
	}
	for _, name := range []string{"data.lua", "prototypes/belt.lua",
		"graphics/icons/thing.png", "control.lua"} {
		if !have[p.DirName()+"/"+name] {
			t.Errorf("zip is missing %s/%s", p.DirName(), name)
		}
	}
	// Zip entry names are slash-separated by spec, whatever the host separator.
	for name := range have {
		if strings.Contains(name, `\`) {
			t.Errorf("zip entry %q uses a backslash", name)
		}
	}
}

// A collision is an ERROR, not a silent winner. Either order of precedence is
// wrong: shadowing control.lua produces a mod whose guest never runs, and
// ignoring the author's file produces one whose data stage silently is not the
// one they wrote.
func TestAnIncludedFileMayNotShadowAGeneratedOne(t *testing.T) {
	for _, name := range []string{"control.lua", "info.json", ABIFile, APIFile,
		GeneratedModuleFile} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p := testPackage()
		if err := p.Include(dir); err != nil {
			t.Fatalf("Include should accept the directory and Files() should refuse: %v", err)
		}
		if _, err := p.Files(); err == nil {
			t.Errorf("%s: an included file shadowed a generated one silently", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: the error should name the file, got: %v", name, err)
		}
	}
}

// Two --include directories contributing the same path is the same problem one
// level out, and it is the shape a Makefile produces by accident.
func TestTwoIncludedDirectoriesMayNotCollide(t *testing.T) {
	p := testPackage()
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "data.lua"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := p.Include(dir); err != nil && i == 0 {
			t.Fatal(err)
		} else if err != nil {
			if !strings.Contains(err.Error(), "data.lua") {
				t.Errorf("the error should name the file, got: %v", err)
			}
			return
		}
	}
	t.Error("two directories both providing data.lua was accepted")
}

// A directory that is not there is an error at package time. Silently shipping
// a mod without its data stage is the failure that costs an author a play
// session to notice.
func TestIncludingAMissingDirectoryFails(t *testing.T) {
	if err := testPackage().Include(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("--include of a missing directory was accepted")
	}
}
