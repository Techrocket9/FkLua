package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
)

// `fklua init NAME --data`: the DATA-STAGE scaffold, in every language the
// project declares.
//
// Until it existed, `init` wrote five files and the string `fkdata` appeared in
// none of them, so an author who wanted a data stage hand-wrote the crate or
// package, hand-added it to the workspace, and worked the build shape out on
// their own. It was deferred on purpose -- scaffolding a shape nobody has used
// is how init shipped a layout three mods each undid by hand -- and the shape
// has now survived two real ports, one per language.
//
// The tests come in two halves. The properties below need no toolchain and are
// about the tree init writes; the end-to-end ones further down run init's own
// printed commands and package the result, which is the only thing that can say
// the scaffold WORKS rather than that it looks right.

// scaffoldFiles lists every file under dir, relative and slash-separated, so a
// file SET can be compared rather than a handful of existence checks.
func scaffoldFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the scaffold wrote no %s: %v", path, err)
	}
	return string(b)
}

// recipeFrom reads the COMMAND BLOCK out of a scaffolded guest's header
// comment -- every indented line under "which is what `fklua init` prints",
// continuations joined, in the order they are written -- so an end-to-end test
// can run the recipe the scaffold hands an author instead of one it spells for
// itself.
//
// A test that builds by its own means proves the toolchain works and says
// nothing about the instruction beside it. That is how a comment reading
// `fklua mod` with no positional shipped one sentence under "package the two
// together" -- fklua.toml names the DATA module, so that command packages a
// data-stage-only mod, with no control.lua and exit 0 -- and then how a recipe
// that built ONE module and packaged TWO shipped one round later: the tests
// built the control guest themselves, so the half the block was missing was
// the half they never read.
//
// EXACTLY ONE `fklua mod` LINE, AND IT IS LAST. Zero is a failure rather than
// an empty command, two is a failure because a silently chosen one of them is
// a test measuring something nobody picked, and a block whose last line is a
// build is a recipe that never packages. Prose mentions are spelled `fklua
// mod` in backticks and do not match.
func recipeFrom(t *testing.T, path string) []string {
	t.Helper()
	var cmds []string
	started, done := false, false
	for _, line := range strings.Split(readFile(t, path), "\n") {
		body := strings.TrimSpace(line)
		if !strings.HasPrefix(body, "//") {
			continue
		}
		if strings.HasPrefix(body, "//!") {
			body = strings.TrimPrefix(body, "//!")
		} else {
			body = strings.TrimPrefix(body, "//")
		}
		// The command block is the INDENTED run: a tab in the Go template, four
		// or more spaces in the Rust one, which is what both languages'
		// formatters render as a code block.
		indented := strings.HasPrefix(body, "\t") || strings.HasPrefix(body, "    ")
		if !indented {
			if started {
				done = true
			}
			continue
		}
		if done {
			continue
		}
		started = true
		cmd := strings.TrimSpace(body)
		// The trailing `# ...` note is a comment in the shell an author pastes
		// into, so it is not part of the command here either -- and stripping
		// it HERE rather than in the runner is what keeps a test that only
		// splits the line and a test that runs it looking at one string.
		if i := strings.Index(cmd, "#"); i >= 0 {
			cmd = strings.TrimSpace(cmd[:i])
		}
		// A continuation line belongs to the command above it.
		if n := len(cmds); n > 0 && strings.HasSuffix(cmds[n-1], "\\") {
			cmds[n-1] = strings.TrimSpace(strings.TrimSuffix(cmds[n-1], "\\")) + " " + cmd
			continue
		}
		cmds = append(cmds, cmd)
	}
	var pkgLines int
	for _, c := range cmds {
		if strings.HasPrefix(c, "fklua mod") {
			pkgLines++
		}
	}
	if pkgLines != 1 || len(cmds) < 3 || !strings.HasPrefix(cmds[len(cmds)-1], "fklua mod") {
		t.Fatalf("%s carries a recipe of %d command(s) with %d `fklua mod` "+
			"line(s), and this test runs the whole of it: it must build the "+
			"control guest, build the data module and package them, in that "+
			"order:\n%v", path, len(cmds), pkgLines, cmds)
	}
	return cmds
}

// shellFields splits one command line into arguments the way the shell an
// author pastes into would, which is why it exists rather than strings.Fields:
// a mod name may carry interior spaces and the artifact is quoted for exactly
// that reason (see shellArg), so a splitter on whitespace would hand runMod two
// positionals and `fklua mod` would refuse them.
//
// Single quotes, double quotes and a backslash escape are all it handles. A
// mod name is matched by factorio.nameRE, which admits no quote and no
// backslash at all, so nothing beyond this can be produced by the code under
// test -- and a quoting rule for a case that cannot arise is a rule nothing
// can check.
func shellFields(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case '\'', '"':
			inWord = true
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				t.Fatalf("unterminated %c in a scaffolded command line: %s", c, s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '\\':
			if i+1 < len(s) {
				i++
				inWord = true
				cur.WriteByte(s[i])
			}
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// runRecipe runs every command the scaffolded header carries, in order, from
// the project root -- the build lines through a shell, exactly as pasted, and
// the `fklua mod` line through runMod so the packaging happens in process and
// can be pointed at a temporary output directory.
//
// CARGO_TARGET_DIR IS REMOVED FROM THE ENVIRONMENT rather than set, because the
// paths in the recipe are cargo's DEFAULT ones. A developer with the variable
// exported would otherwise watch the artifacts land somewhere the printed
// `fklua mod` line cannot see, and read a correct recipe as a broken one.
func runRecipe(t *testing.T, dir, path, out string) []string {
	t.Helper()
	return runCommands(t, dir, path, recipeFrom(t, path), out)
}

// runCommands is runRecipe's other half, split out so a recipe that was not
// read from a file can be run the same way. what names where the commands came
// from, for the failure messages.
func runCommands(t *testing.T, dir, what string, cmds []string, out string) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CARGO_TARGET_DIR=") {
			env = append(env, kv)
		}
	}
	var args []string
	for _, c := range cmds {
		if !strings.HasPrefix(c, "fklua mod") {
			cmd := exec.Command("sh", "-c", c)
			cmd.Dir = dir
			cmd.Env = env
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("a line of the recipe in %s does not run:\n"+
					"  %s\n%v\n%s", what, c, err, b)
			}
			continue
		}
		args = shellFields(t, c)[2:]
		if err := runMod(append(append([]string{}, args...), "-o", out)); err != nil {
			t.Fatalf("`%s`, which is what %s tells the author to run, failed: %v",
				c, what, err)
		}
	}
	return args
}

// THE SCAFFOLD'S PROPERTIES, in both languages at once, with no toolchain.
//
// --lang go,rust so that the two arms are covered by ONE run rather than by two
// that could disagree about the manifest they share, which is the same reason
// TestInitWritesAGitignoreForItsOwnBuildOutput takes both.
func TestInitDataScaffoldsADataGuestInEveryLanguage(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "data-mod"
	if err := runInit([]string{modName, "--lang", "go,rust", "--data"}); err != nil {
		t.Fatalf("fklua init --data: %v", err)
	}

	// 1. THE MANIFEST NAMES A DATA MODULE, and it names it in a form the parser
	//    this project ships reads back. Rendering and parsing are separate code,
	//    and a key that only round-trips through the writer works until somebody
	//    reads it.
	raw := readFile(t, projectFile)
	proj, err := factorio.ParseProject(raw)
	if err != nil {
		t.Fatalf("init wrote an %s its own parser rejects: %v\n%s", projectFile, err, raw)
	}
	// `lang = ["go", "rust"]`, so the key takes the FIRST and the other rides
	// beside it as a comment. One key however many languages: a mod has one
	// data stage.
	if proj.DataModule != GoDataArtifact(modName) {
		t.Errorf("data_module = %q, want %q -- the key takes the first language "+
			"in lang order", proj.DataModule, GoDataArtifact(modName))
	}
	rustArtifact := rustReleaseWasm(RustDataArtifact(modName))
	if !strings.Contains(raw, "#   "+rustArtifact) {
		t.Errorf("the manifest does not name the Rust data artifact beside the "+
			"key, so a two-language project is never told where its other build "+
			"lands:\n%s", raw)
	}
	if n := strings.Count(raw, "data_module ="); n != 1 {
		t.Errorf("the manifest declares data_module %d times; a mod has exactly "+
			"ONE data stage:\n%s", n, raw)
	}

	// 2. THE GO DATA GUEST is a second main package inside the control guest's
	//    OWN module, which is what lets a shared sibling package serve both
	//    stages, and it hooks a stage.
	goMain := readFile(t, filepath.Join(goDataGuestDir, "main.go"))
	for _, want := range []string{
		"package main",
		"//go:wasmexport fk_data",
		"guest/go/fkdata",
		"fkdata.ModName()",
		// The build line, and the flag that differs from the control guest's.
		"-gc=leaking",
	} {
		if !strings.Contains(goMain, want) {
			t.Errorf("%s does not contain %q:\n%s",
				filepath.Join(goDataGuestDir, "main.go"), want, goMain)
		}
	}
	if strings.Contains(goMain, "fkapi") && !strings.Contains(goMain, "NEVER IMPORT fkapi") {
		t.Errorf("the Go data guest mentions fkapi outside its own warning:\n%s", goMain)
	}
	if _, err := os.Stat(filepath.Join(goDataGuestDir, "gc.go")); err == nil {
		t.Errorf("%s exists. A data module is compiled -gc=leaking whatever the "+
			"control guest uses, and packaging refuses one that carries a "+
			"collector, so a gc.go here is a build that will be refused",
			filepath.Join(goDataGuestDir, "gc.go"))
	}

	// 3. THE RUST DATA CRATE depends on fkdata and NOT on fkapi, and it is a
	//    workspace member so it shares the lockfile.
	crateDir := rustDataCrateDir(modName)
	crateCargo := readFile(t, filepath.Join(crateDir, "Cargo.toml"))
	if !strings.Contains(crateCargo, "fkdata = { workspace = true }") {
		t.Errorf("the data crate does not depend on fkdata, so it cannot reach "+
			"data.raw at all:\n%s", crateCargo)
	}
	if strings.Contains(crateCargo, "fkapi = {") {
		t.Errorf("the data crate depends on fkapi. There is no runtime API at a "+
			"data stage and `fklua mod` refuses a data module that imports "+
			"one:\n%s", crateCargo)
	}
	if strings.Contains(crateCargo, "[features]") || strings.Contains(crateCargo, "fkgc") &&
		!strings.Contains(crateCargo, "never `fk/fkgc`") {
		t.Errorf("the data crate declares a feature table or the collector "+
			"feature:\n%s", crateCargo)
	}
	ws := readFile(t, filepath.Join(rustGuestDir, "Cargo.toml"))
	wantMembers := `members = ["fkapi", "` + rustCrateName(modName) + `", "` +
		rustDataCrateName(modName) + `"]`
	if !strings.Contains(ws, wantMembers) {
		t.Errorf("the workspace is not %s:\n%s", wantMembers, ws)
	}
	// fkdata reaches workspace.dependencies the way fk does, and fklog with it:
	// every guest past its first week hand-adds a line builder.
	for _, want := range []string{"fkdata = {", "fklog = {"} {
		if !strings.Contains(ws, want) {
			t.Errorf("the workspace declares no %q, so the data crate's dependency "+
				"has nowhere to resolve from:\n%s", want, ws)
		}
	}
	crateLib := readFile(t, filepath.Join(crateDir, "src", "lib.rs"))
	for _, want := range []string{
		"#[no_mangle]",
		"pub extern \"C\" fn fk_data()",
		"fkdata::mod_name()",
		// The one thing about the build that is not obvious, in the two forms
		// the header states it. Both are UNCONDITIONAL, and that is why they
		// are the strings asserted: the header used to open "EACH IN ITS OWN
		// CARGO INVOCATION" over a block whose second line is a tinygo command
		// whenever the key names Go, which is the arm this very test
		// scaffolds. The build LINES are asserted below, as commands rather
		// than as substrings, because they wrap.
		"NEVER IN ONE CARGO INVOCATION",
		"THE -p IS THE LOAD-BEARING PART",
	} {
		if !strings.Contains(crateLib, want) {
			t.Errorf("%s does not contain %q:\n%s",
				filepath.Join(crateDir, "src", "lib.rs"), want, crateLib)
		}
	}

	// ...and the recipe in that header is three commands in init's order:
	// control, data, package. Read as COMMANDS rather than as substrings, since
	// the build lines wrap.
	//
	// `lang = ["go", "rust"]` HERE, SO LINE 2 IS A TINYGO LINE IN A RUST FILE,
	// and that is the whole point of it: `data_module` takes the FIRST language,
	// so the module this header's packaging line reads is the GO one and a
	// block that built this crate's own data module instead would end on an
	// open error naming a wasm nothing in it had built.
	wantRecipe := []string{
		"(cd " + rustGuestDir + " && cargo build --release --target " +
			"wasm32-unknown-unknown -p " + rustCrateName(modName) +
			" --features fk/fkgc)",
		"(cd " + guestDir + " && tinygo build -target=wasm-unknown " +
			"-scheduler=none -gc=leaking -opt=2 -o ../../" +
			GoDataArtifact(modName) + " ./data)",
		"fklua mod " + rustReleaseWasm(RustGuestArtifact(modName)),
	}
	got := recipeFrom(t, filepath.Join(crateDir, "src", "lib.rs"))
	if len(got) != len(wantRecipe) {
		t.Fatalf("the Rust data crate's recipe is %v, want %v", got, wantRecipe)
	}
	for i := range wantRecipe {
		if got[i] != wantRecipe[i] {
			t.Errorf("recipe line %d is\n  %s\nwant\n  %s", i+1, got[i], wantRecipe[i])
		}
	}

	// 4. GIT KNOWS ABOUT THE SECOND ARTIFACT. The Go data wasm lands at the
	//    project root beside the control guest's, and the pattern above it is
	//    anchored and exact rather than a glob, so it needs its own line. The
	//    Rust one is inside the already-ignored target tree.
	ignore := readFile(t, gitignoreFile)
	if !strings.Contains(ignore, "\n/"+GoDataArtifact(modName)+"\n") {
		t.Errorf("%s has no pattern for the Go data module, so `git status` in a "+
			"fresh project is noise the moment its own next steps are run:\n%s",
			gitignoreFile, ignore)
	}
}

// WITHOUT --data THE SCAFFOLD IS WHAT IT ALWAYS WAS, and this is the assertion
// that makes the flag an addition rather than a change.
//
// Two halves, because neither alone says it. The first is the file SET plus the
// absence of every token the data scaffold introduces -- `fkdata` appearing in
// none of the files init writes is the gap's own wording. The second is BYTE
// identity of every file the flag has no business touching, compared against a
// --data run of the same mod in a sibling directory: three files legitimately
// differ (the manifest gains a key, .gitignore a pattern, the workspace a
// member), and every other file must be identical to the byte.
func TestInitWithoutDataScaffoldsNoDataStage(t *testing.T) {
	const modName = "plain-mod"

	plain := t.TempDir()
	back := chdir(t, plain)
	if err := runInit([]string{modName, "--lang", "go,rust"}); err != nil {
		back()
		t.Fatalf("fklua init: %v", err)
	}
	back()

	withData := t.TempDir()
	back = chdir(t, withData)
	if err := runInit([]string{modName, "--lang", "go,rust", "--data"}); err != nil {
		back()
		t.Fatalf("fklua init --data: %v", err)
	}
	back()

	// The EIGHT files a `--lang go,rust` scaffold has always written, and no
	// ninth. Five is the one-language count and this test runs both arms at
	// once, which is the whole reason it can see a data crate appear beside a
	// data package.
	want := []string{
		".gitignore",
		"fklua.toml",
		"guest/go/gc.go",
		"guest/go/go.mod",
		"guest/go/main.go",
		"guest/rust/" + rustCrateName(modName) + "/Cargo.toml",
		"guest/rust/" + rustCrateName(modName) + "/src/lib.rs",
		"guest/rust/Cargo.toml",
	}
	got := scaffoldFiles(t, plain)
	if len(got) != len(want) {
		t.Errorf("a scaffold with no --data writes %d files:\n%v\nwant\n%v",
			len(got), got, want)
	}
	have := map[string]bool{}
	for _, f := range got {
		have[f] = true
	}
	for _, f := range want {
		if !have[f] {
			t.Errorf("a scaffold with no --data is missing %s", f)
		}
	}

	// NOT ONE MENTION OF THE DATA STAGE, which is the gap's own measure: the
	// string `fkdata` appeared in none of the five files init wrote, and
	// without the flag it still does not.
	for _, f := range got {
		body := readFile(t, filepath.Join(plain, f))
		for _, never := range []string{"fkdata", "data_module",
			GoDataArtifact(modName), rustDataCrateName(modName)} {
			if strings.Contains(body, never) {
				t.Errorf("%s mentions %q without --data:\n%s", f, never, body)
			}
		}
	}

	// BYTE IDENTITY for everything --data has no business touching.
	moved := map[string]bool{
		"fklua.toml":            true, // gains data_module
		".gitignore":            true, // gains the Go data artifact
		"guest/rust/Cargo.toml": true, // gains a member and two dependencies
	}
	for _, f := range got {
		if moved[f] {
			continue
		}
		a := readFile(t, filepath.Join(plain, f))
		b := readFile(t, filepath.Join(withData, f))
		if a != b {
			t.Errorf("--data changed %s, which is not one of the three files it "+
				"has a reason to touch:\n--- without\n%s\n--- with\n%s", f, a, b)
		}
	}
	// ...and the three that DO differ must really differ, or the comparison
	// above is passing over a flag that did nothing.
	for f := range moved {
		if readFile(t, filepath.Join(plain, f)) == readFile(t, filepath.Join(withData, f)) {
			t.Errorf("--data left %s byte-identical, so the flag did nothing there", f)
		}
	}
}

// controlLine, dataLine and controlArtifactPath are what a scaffolded header's
// three recipe commands have to say, spelled out here from each language's own
// build line rather than asked of the code that writes them. A test that read
// the generator would agree with a generator that had written the same wrong
// block into both headers, which is the shape this file already refuses for the
// key note.
func controlLine(modName, lang string) string {
	if lang == "rust" {
		return "(cd " + rustGuestDir + " && cargo build --release --target " +
			"wasm32-unknown-unknown -p " + rustCrateName(modName) +
			" --features fk/fkgc)"
	}
	return "(cd " + guestDir + " && tinygo build -target=wasm-unknown " +
		"-scheduler=none -gc=custom -opt=2 -o ../../" +
		GoGuestArtifact(modName) + " .)"
}

func dataLine(modName, lang string) string {
	if lang == "rust" {
		return "(cd " + rustGuestDir + " && cargo build --release --target " +
			"wasm32-unknown-unknown -p " + rustDataCrateName(modName) + ")"
	}
	return "(cd " + guestDir + " && tinygo build -target=wasm-unknown " +
		"-scheduler=none -gc=leaking -opt=2 -o ../../" +
		GoDataArtifact(modName) + " ./data)"
}

func controlArtifactPath(modName, lang string) string {
	if lang == "rust" {
		return rustReleaseWasm(RustGuestArtifact(modName))
	}
	return GoGuestArtifact(modName)
}

// WHICH DATA MODULE THE KEY NAMES IS SAID IN THE SCAFFOLDED FILE, and it is
// said correctly in a two-language project, which is the case a constant
// sentence cannot cover.
//
// `data_module` takes the FIRST language in `lang` order, so "it names this
// module" is false in exactly half the two-language projects -- and it was
// false in a file whose next two lines are a command that packages the other
// one. Both notes are generated from dataArtifacts, the same fact init's
// printed steps are generated from, so this runs BOTH orders and asserts each
// file names the artifact the key really holds.
//
// AND THE RECIPE IS ASSERTED WITH THE NOTE, because reading the note alone is
// what let the recipe stay wrong: the note was generated and correct while the
// block three lines above it was written entirely in its own language, so the
// non-key-holding header built a data module the packaging line never reads and
// then opened one nothing in the block had built. The expectations below are
// hand-written from the two languages' own build lines rather than from the
// generator, and TestEitherScaffoldedRecipeRunsInATwoLanguageProject RUNS them.
func TestTheScaffoldedDataGuestsSayWhichModuleTheKeyNames(t *testing.T) {
	for _, tc := range []struct {
		langs string
		// The language whose data artifact `data_module` ends up holding.
		key string
	}{
		{"go,rust", "go"},
		{"rust,go", "rust"},
	} {
		t.Run(tc.langs, func(t *testing.T) {
			dir := t.TempDir()
			back := chdir(t, dir)
			defer back()

			const modName = "key-note-mod"
			if err := runInit([]string{modName, "--lang", tc.langs, "--data"}); err != nil {
				t.Fatalf("fklua init --data: %v", err)
			}
			proj, err := factorio.ParseProject(readFile(t, projectFile))
			if err != nil {
				t.Fatal(err)
			}
			goPath := GoDataArtifact(modName)
			rustPath := rustReleaseWasm(RustDataArtifact(modName))
			held, other := goPath, rustPath
			if tc.key == "rust" {
				held, other = rustPath, goPath
			}
			// THE PREMISE, asserted rather than assumed: everything below is
			// about a key whose value this test believes it knows.
			if proj.DataModule != held {
				t.Fatalf("data_module = %q, want %q -- `lang = %q` gives the key "+
					"the first language", proj.DataModule, held, tc.langs)
			}

			for _, f := range []struct{ lang, path string }{
				{"go", filepath.Join(goDataGuestDir, "main.go")},
				{"rust", filepath.Join(rustDataCrateDir(modName), "src", "lib.rs")},
			} {
				body := readFile(t, f.path)
				// THE VACUITY TOOTH. A file that stopped saying anything about
				// the key would satisfy every refusal below.
				if !strings.Contains(body, "`data_module`") {
					t.Fatalf("%s never mentions the key, so this test asserts "+
						"nothing about it:\n%s", f.path, body)
				}
				// The note has to name the path the key actually holds,
				// whichever file it is in: an author reading either one is
				// about to run a `fklua mod` line that packages that module.
				if !strings.Contains(body, held) {
					t.Errorf("%s never names %s, which is what data_module "+
						"holds and therefore what every `fklua mod` line in "+
						"this project packages:\n%s", f.path, held, body)
				}
				if !strings.Contains(body, other) {
					t.Errorf("%s never names %s, so the one-line edit that "+
						"switches which language ships is not written "+
						"anywhere:\n%s", f.path, other, body)
				}
				claimsTheKey := strings.Contains(body, "THE KEY NAMES THIS MODULE")
				if want := f.lang == tc.key; claimsTheKey != want {
					t.Errorf("%s says the key names this module = %v, and it "+
						"is %v: `lang = %q` gives the key the %s build "+
						"(%s):\n%s", f.path, claimsTheKey, want, tc.langs,
						langName(tc.key), held, body)
				}

				// THE RECIPE IN THAT HEADER, as commands: this file's own
				// control guest, the data module the KEY names whatever
				// language that is, and a packaging line taking this file's
				// control artifact. Every string here is spelled out rather
				// than asked of the generator, so a generator that produced
				// the same wrong block twice could not satisfy it.
				want := []string{
					controlLine(modName, f.lang),
					dataLine(modName, tc.key),
					"fklua mod " + controlArtifactPath(modName, f.lang),
				}
				got := recipeFrom(t, f.path)
				if len(got) != len(want) {
					t.Fatalf("%s carries a recipe of %v, want %v", f.path, got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("%s recipe line %d is\n  %s\nwant\n  %s\n"+
							"(line 2 builds the module `data_module` names, "+
							"which `lang = %q` gives to %s; a literal paste "+
							"that builds this file's own language instead ends "+
							"on an open error naming a wasm nothing in the "+
							"block ever built)",
							f.path, i+1, got[i], want[i], tc.langs,
							langName(tc.key))
					}
				}
			}
		})
	}
}

// A LANGUAGE DECLARED TWICE IS ONE LANGUAGE, which is the only reading under
// which everything downstream of `lang` stays true.
//
// `--lang go,go` is a typo, and it used to reach the manifest whole. Every
// reader of `lang` then saw a TWO-language project: `data_module` took the
// first entry and the "other" artifact rode beside it as a comment naming the
// same file, and the scaffolded note read "THE KEY DOES NOT NAME THIS MODULE"
// about a key holding that module's own path. The duplicate is dropped where
// init parses the flag, so one language is one language in the manifest, in the
// printed steps and in the note alike.
func TestALanguageDeclaredTwiceIsOneLanguage(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "dupe-lang-mod"
	if err := runInit([]string{modName, "--lang", "go,go", "--data"}); err != nil {
		t.Fatalf("fklua init --lang go,go --data: %v", err)
	}
	raw := readFile(t, projectFile)
	proj, err := factorio.ParseProject(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(proj.Langs) != 1 || proj.Langs[0] != "go" {
		t.Errorf("lang = %q, want [go]: a language named twice is one "+
			"language:\n%s", proj.Langs, raw)
	}
	// THE MANIFEST TEXT, not the parsed struct, and that distinction is the
	// whole assertion. There is no `data_module_alt` KEY: the other language's
	// artifact goes out as a `#` COMMENT beside the key, so ParseProject never
	// sets Project.DataModuleAlt and it is the zero value for every manifest
	// ever written, defect or none. An assertion on it is one that cannot
	// fail, which is worse than no assertion at all -- this test read the raw
	// manifest already, and the symptom lives there.
	//
	// THE KEY IS THE VACUITY TOOTH, and it is asserted here rather than assumed:
	// both strings below are absent from a manifest that declares no data
	// module at all, so their absence says nothing until the key is known to be
	// there and to hold the one language's own build.
	if proj.DataModule != GoDataArtifact(modName) {
		t.Fatalf("data_module = %q, want %q; the two absences below are "+
			"vacuous over a manifest with no data module in it:\n%s",
			proj.DataModule, GoDataArtifact(modName), raw)
	}
	for _, never := range []string{
		// The comment the two-language branch of the renderer opens with.
		"The other lands at:",
		// ...and the path it would name, which in a `--lang go,go` project is
		// the key's OWN module: one file described as two.
		"#   " + proj.DataModule,
	} {
		if strings.Contains(raw, never) {
			t.Errorf("the manifest carries %q, so it names a SECOND language's "+
				"data artifact in a project with one language -- and the path "+
				"it names is the one the key already holds (%q):\n%s",
				never, proj.DataModule, raw)
		}
	}
	// The note is the sentence an author reads three lines above the recipe.
	body := readFile(t, filepath.Join(goDataGuestDir, "main.go"))
	if !strings.Contains(body, "THE KEY NAMES THIS MODULE") {
		t.Errorf("%s does not say the key names this module, and the key holds "+
			"%s, which is this module:\n%s",
			filepath.Join(goDataGuestDir, "main.go"), proj.DataModule, body)
	}
	if strings.Contains(body, "THE KEY DOES NOT NAME THIS MODULE") {
		t.Errorf("%s says the key does not name this module, about a key "+
			"holding %s:\n%s", filepath.Join(goDataGuestDir, "main.go"),
			proj.DataModule, body)
	}
}

// A MOD NAME WITH A SPACE STAYS ONE ARGUMENT, in the printed steps and in the
// scaffolded header alike.
//
// factorio.nameRE admits interior spaces, so `fklua init "my mod"` used to
// print `fklua mod my mod.wasm` -- which the command refuses as two
// positionals -- and this flag is what turned that from a printed line into a
// line written into a source file that says it is what init prints, and read
// back by the test above. The quoting is a SHELL quoting: the .gitignore
// pattern is asserted here to be bare, because git matches it literally.
func TestASpacedModNameIsQuotedWhereverAShellReadsIt(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "my mod"
	out := captureStdout(t, func() error {
		return runInit([]string{modName, "--lang", "go", "--data"})
	})

	quoted := "'" + GoGuestArtifact(modName) + "'"
	quotedData := "'" + GoDataArtifact(modName) + "'"
	for _, want := range []string{
		"  fklua mod " + quoted + "\n",
		"-o ../../" + quoted + " .)",
		"-o ../../" + quotedData + " ./data)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("init's printed steps do not carry %q, so a name with a "+
				"space is printed as a command that refuses itself:\n%s", want, out)
		}
	}

	// AND THE SCAFFOLDED RECIPE SPLITS THE WAY A SHELL SPLITS IT, which is the
	// half a line-counting tooth cannot see: an unquoted artifact is one line
	// carrying two positionals.
	main := filepath.Join(goDataGuestDir, "main.go")
	cmds := recipeFrom(t, main)
	args := shellFields(t, cmds[len(cmds)-1])
	if len(args) != 3 || args[2] != GoGuestArtifact(modName) {
		t.Errorf("the scaffolded `fklua mod` line splits into %q; a mod name "+
			"with a space has to reach the command as ONE positional", args)
	}

	// THE PATTERN IS BARE, deliberately: git has no quoting and would take the
	// quotes for characters in the filename.
	ignore := readFile(t, gitignoreFile)
	if !strings.Contains(ignore, "\n/"+GoGuestArtifact(modName)+"\n") {
		t.Errorf("%s has no bare pattern for the guest wasm:\n%s", gitignoreFile, ignore)
	}
	if strings.Contains(ignore, "\n/"+quoted+"\n") {
		t.Errorf("%s quotes its own pattern, which makes it match a filename "+
			"with quotes in it:\n%s", gitignoreFile, ignore)
	}
}

// THE DATA MODULE'S ONE-SENTENCE DEFINITION IS LANGUAGE-NEUTRAL EVERYWHERE IT
// IS READ, and there are five places it is read.
//
// "compiled from its own main package" was right while Go was the only
// language that could have a data module, and `fklua init --lang rust --data`
// is what made it wrong: there is no main package in a Rust project. It was
// fixed in all five copies at once and nothing held them together, so the next
// language-specific sentence in that block could go wrong the same way with
// everything green. This is the guard, in the shape
// TestTheScaffoldedManifestDoesNotDenyRustACollector already has: the folded
// text, the refused phrasings, and a vacuity tooth per site.
//
// guest/go/fkdata's own copy is deliberately NOT a site. That library is Go,
// and its header is allowed to say so.
func TestTheDataModuleSentenceIsLanguageNeutralWhereverItIsRead(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()
	if err := runInit([]string{"neutral-mod", "--lang", "rust", "--data",
		"--guest-module", filepath.Join(root, "guest", "rust")}); err != nil {
		t.Fatalf("fklua init --data: %v", err)
	}

	sites := []struct{ what, body string }{
		// The two RENDERED forms, which is what an author actually reads.
		{"the scaffolded fklua.toml", readFile(t, projectFile)},
		{"`fklua mod --help`", usage},
		// ...and the sources the rest of the copies live in, because a comment
		// that goes wrong is read by whoever changes the code next.
		{"cmd/fklua/main.go", readFile(t, filepath.Join(root, "cmd", "fklua", "main.go"))},
		{"internal/factorio/project.go", readFile(t, filepath.Join(root, "internal", "factorio", "project.go"))},
		{"internal/factorio/mod.go", readFile(t, filepath.Join(root, "internal", "factorio", "mod.go"))},
		// The SIXTH, and it is the one that shows a refusal list is not a
		// property: this file says the same thing about the same object, in
		// the file that ships fk_data.lua for BOTH languages, and its wording
		// ("compiled from a different main package") matched none of the four
		// phrasings below. It escaped the guard by one word.
		{"runtime/embed.go", readFile(t, filepath.Join(root, "runtime", "embed.go"))},
	}
	if len(sites) != 6 {
		t.Fatalf("the sentence had six copies and this test covers %d", len(sites))
	}
	for _, site := range sites {
		// THE VACUITY TOOTH, per site: a file that stopped defining a data
		// module at all would satisfy every refusal below.
		if !strings.Contains(site.body, "built from its own package or crate") {
			t.Errorf("%s never says what a data module is built from, so there "+
				"is no claim left here to hold to either language", site.what)
		}
		for _, never := range []string{
			"its own main package",
			"compiled from its own main package",
			"a second main package",
			"must be a main package",
			"a different main package",
		} {
			if strings.Contains(site.body, never) {
				t.Errorf("%s says %q. A Rust data module is a cdylib crate, and "+
					"`fklua init --lang rust --data` is what writes this "+
					"sentence into a Rust-only project", site.what, never)
			}
		}
	}
}

// A MISSING DATA ARTIFACT NAMES WHERE ITS PATH CAME FROM, which is new because
// this flag is: init now writes data_module into every --data project, so an
// author who runs the printed build lines out of order meets a missing data
// artifact before they meet anything else. It used to be a bare
// `open x-data.wasm: no such file or directory`, naming neither the key that
// produced the path nor the fact that a data module has a build line of its
// own.
//
// No toolchain here on purpose: nothing is built, which is the case under test.
func TestAMissingDataArtifactNamesWhereItsPathCameFrom(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "missing-art-mod"
	if err := runInit([]string{modName, "--lang", "go", "--data"}); err != nil {
		t.Fatalf("fklua init --data: %v", err)
	}

	// No positional: a control guest is optional when the mod has a data
	// module, so this reaches the data module's own load and nothing else.
	err := runMod([]string{"-o", filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("packaging a project whose data artifact was never built succeeded")
	}
	for _, want := range []string{GoDataArtifact(modName), projectFile,
		"[fklua] data_module"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a missing data artifact does not mention %q:\n%v", want, err)
		}
	}

	// AND THE FLAG BLAMES THE FLAG. The same failure reached through
	// --data-module must not send an author to a manifest key they overrode.
	err = runMod([]string{"--data-module", "never-built.wasm",
		"-o", filepath.Join(dir, "out2")})
	if err == nil {
		t.Fatal("packaging with --data-module naming no file succeeded")
	}
	if !strings.Contains(err.Error(), "--data-module") {
		t.Errorf("a missing --data-module artifact does not name the flag:\n%v", err)
	}
	if strings.Contains(err.Error(), "[fklua] data_module") {
		t.Errorf("a missing --data-module artifact blames the manifest key the "+
			"command overrode:\n%v", err)
	}

	// AND A FILE THAT IS THERE IS NOT TOLD TO BUILD IT. The provenance half is
	// right for every failure and is why the wrapper exists; the remedy is
	// about a MISSING artifact, and appending it to `bad wasm magic` tells an
	// author to re-run a build that already produced the file they are looking
	// at.
	if err := os.WriteFile(GoDataArtifact(modName), []byte("not a wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = runMod([]string{"-o", filepath.Join(dir, "out3")})
	if err == nil {
		t.Fatal("packaging a data artifact that is not a wasm module succeeded")
	}
	if !strings.Contains(err.Error(), "[fklua] data_module") {
		t.Errorf("a data artifact that will not decode stops saying where its "+
			"path came from, which is the half that is right in every case:\n%v", err)
	}
	if strings.Contains(err.Error(), "build it before packaging") {
		t.Errorf("a data artifact that IS there is told to build it before "+
			"packaging:\n%v", err)
	}
}

// AND SO DOES A MISSING CONTROL ARTIFACT, which this flag is equally what makes
// reachable: `fklua init --data` prints a THREE-line recipe and scaffolds it
// into both data guests' headers, so a paste that stopped at the second line --
// or ran the two builds in the other order -- reaches the packaging command
// with no control guest built. It used to be a bare `open my-mod.wasm: no such
// file or directory`, which is the shape the data module's own failure was
// wrapped out of one round earlier.
//
// No toolchain here on purpose: nothing is built, which is the case under test.
func TestAMissingControlArtifactNamesWhereItsPathCameFrom(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "missing-control-mod"
	if err := runInit([]string{modName, "--lang", "go", "--data"}); err != nil {
		t.Fatalf("fklua init --data: %v", err)
	}

	err := runMod([]string{GoGuestArtifact(modName), "-o", filepath.Join(dir, "out")})
	if err == nil {
		t.Fatal("packaging a project whose control guest was never built succeeded")
	}
	for _, want := range []string{GoGuestArtifact(modName), "control module",
		"positional", "`fklua init`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("a missing control artifact does not mention %q:\n%v", want, err)
		}
	}
	// AND IT DOES NOT BLAME THE KEY. There is no manifest key for the control
	// module, which is the asymmetry the scaffolded headers spend a paragraph
	// on; sending an author to data_module for a path they typed themselves
	// would be the same defect the data module's own message exists to avoid,
	// pointed the other way.
	if strings.Contains(err.Error(), "[fklua] data_module") {
		t.Errorf("a missing control artifact blames the data module's manifest "+
			"key, which never named it:\n%v", err)
	}

	// AND A FILE THAT IS THERE IS NOT TOLD TO BUILD IT, the same gate the data
	// module's own wrapper carries one door over.
	if err := os.WriteFile(GoGuestArtifact(modName), []byte("not a wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = runMod([]string{GoGuestArtifact(modName), "-o", filepath.Join(dir, "out2")})
	if err == nil {
		t.Fatal("packaging a control artifact that is not a wasm module succeeded")
	}
	if !strings.Contains(err.Error(), "positional") {
		t.Errorf("a control artifact that will not decode stops saying where "+
			"its path came from:\n%v", err)
	}
	if strings.Contains(err.Error(), "build it before packaging") {
		t.Errorf("a control artifact that IS there is told to build it before "+
			"packaging:\n%v", err)
	}
}

// --data AND --no-guest CONTRADICT, and the refusal is the same shape --library
// and --no-guest already have: a data guest is a second module in the SAME tree
// as the control one, so there is nothing to scaffold it beside.
func TestInitDataAndNoGuestContradict(t *testing.T) {
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	err := runInit([]string{"nope-mod", "--data", "--no-guest"})
	if err == nil {
		t.Fatal("--data --no-guest was accepted")
	}
	for _, want := range []string{"--no-guest", "data_module"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(projectFile); statErr == nil {
		t.Error("the refusal still wrote a manifest")
	}
}

// END TO END, GO: init's own printed commands, run in init's order, and the
// packaged mod as the verdict.
//
// The same shape as TestAFreshInitProjectBuildsAndPackagesCollected, and for
// the same reason: a test that checks the printed advice is a test of a string.
// This one runs it. --guest-module points the scaffold at this checkout so the
// run needs no network, which is a flag on the init invocation rather than an
// edit to anything init produced.
func TestAFreshInitDataProjectBuildsAndPackagesBothModules(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this builds two guests with tinygo")
	}
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "data-e2e-mod"
	if err := runInit([]string{modName, "--lang", "go", "--data",
		"--guest-module", root}); err != nil {
		t.Fatalf("fklua init --data: %v", err)
	}

	// THE WHOLE RECIPE COMES OUT OF THE SCAFFOLD, not out of this test: both
	// tinygo lines and the packaging line are read from
	// guest/go/data/main.go's own header and run in the order it writes them.
	// Nothing here builds anything by other means, which is the part that makes
	// the instruction stay fixed -- a test that built the control guest itself
	// would read a block whose first line it never ran, and a recipe missing
	// that line would package nothing but an open error.
	//
	// Everything else about the packaging comes from the manifest: the data
	// module can only have come from fklua.toml's data_module key, the gc mode
	// only from its gc key, and the control guest only from the positional the
	// comment names. control.lua below is what catches a comment that stops
	// naming it, since `fklua mod` with no positional is a legal
	// data-stage-only package rather than an error.
	out := filepath.Join(dir, "out")
	runRecipe(t, dir, filepath.Join(dir, goDataGuestDir, "main.go"), out)
	pkg := filepath.Join(out, modName+"_0.1.0")
	for _, want := range []string{factorio.DataStageFile, factorio.DataModuleFile,
		"data.lua", "control.lua"} {
		if _, err := os.Stat(filepath.Join(pkg, want)); err != nil {
			t.Errorf("the packaged mod has no %s: %v", want, err)
		}
	}
	// The generated stage file calls the guest with the mod's own name, which
	// is what fkdata.ModName() hands the scaffolded guest back.
	body := readFile(t, filepath.Join(pkg, "data.lua"))
	if !strings.Contains(body, `require("fk_data").run(2, "`+modName+`")`) {
		t.Errorf("data.lua does not call the guest's data hook:\n%s", body)
	}
	// A hook the guest does not export gets no file, which is the same
	// feature-detection discipline control.lua applies to fk_on_tick.
	if _, err := os.Stat(filepath.Join(pkg, "settings.lua")); err == nil {
		t.Error("the scaffolded data guest exports no fk_settings and a " +
			"settings.lua was generated anyway")
	}
}

// END TO END, RUST -- and the second half of it is where this change and the
// collector refusal prove each other.
//
// The data crate depends on fkdata alone, so `-p <crate>-data --features
// fk/fkgc` is not even a legal command: fk is not a direct dependency of that
// package. What IS legal, and is the mistake this whole shape exists to
// prevent, is ONE cargo invocation covering both crates -- which is what a
// person types when they want to build their mod. The v2 resolver unifies the
// feature across it and the data module comes out carrying the collector.
func TestAFreshInitDataRustProjectBuildsAndTheUnifiedBuildIsRefused(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	back := chdir(t, dir)
	defer back()

	const modName = "data-rs-e2e-mod"
	if err := runInit([]string{modName, "--lang", "rust", "--data",
		"--guest-module", filepath.Join(root, "guest", "rust")}); err != nil {
		t.Fatalf("fklua init --lang rust --data: %v", err)
	}
	t.Setenv("FKLUA_API_DIR", filepath.Join(root, "api"))
	if err := runGenBindings([]string{"--lang=rust"}); err != nil {
		t.Fatalf("fklua gen-bindings, which is init's own next step: %v", err)
	}

	// TWO INVOCATIONS, ONE PER MODULE, AND THEY COME OUT OF THE SCAFFOLD --
	// exactly as on the Go arm: src/lib.rs's header is what an author copies,
	// and running the whole of it is what says the recipe works. The two cargo
	// lines write into the workspace's DEFAULT target directory, which is where
	// the packaging line's path points, so nothing has to be moved afterwards.
	out := filepath.Join(dir, "out")
	args := runRecipe(t, dir, filepath.Join(dir, rustDataCrateDir(modName),
		"src", "lib.rs"), out)
	if len(args) != 1 || args[0] != rustReleaseWasm(RustGuestArtifact(modName)) {
		t.Fatalf("the recipe packages %v; the positional is the CONTROL guest's "+
			"cdylib at %s", args, rustReleaseWasm(RustGuestArtifact(modName)))
	}
	control := filepath.Join(dir, rustReleaseWasm(RustGuestArtifact(modName)))
	pkg := filepath.Join(out, modName+"_0.1.0")
	for _, want := range []string{"control.lua", factorio.DataStageFile,
		factorio.DataModuleFile, "data.lua"} {
		if _, err := os.Stat(filepath.Join(pkg, want)); err != nil {
			t.Errorf("the packaged mod has no %s: %v", want, err)
		}
	}

	// THE MISTAKE, REPRODUCED. Spelled out here rather than added to
	// internal/guest, because a helper for it would be a supported way to do
	// the wrong thing; the flag and the target come from internal/guest all the
	// same, so the one part that could drift does not.
	unified := filepath.Join(dir, "cargo-unified")
	cmd := exec.Command("cargo", "build", "--release", "--target", guest.RustTarget,
		"-p", rustCrateName(modName), "-p", rustDataCrateName(modName),
		"--features", guest.RustCollectorFeature)
	cmd.Dir = rustGuestDir
	cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+unified)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the one-invocation build did not even run, so nothing below "+
			"measures anything: %v\n%s", err, outBytes)
	}
	infected := filepath.Join(unified, guest.RustTarget, "release",
		RustDataArtifact(modName))
	if _, err := os.Stat(infected); err != nil {
		t.Fatalf("the unified build wrote no data module: %v", err)
	}
	err = runMod([]string{control, "--data-module", infected,
		"-o", filepath.Join(dir, "out-infected")})
	if err == nil {
		t.Fatal("a data module built in the same cargo invocation as a collected " +
			"control guest packaged happily. That is the whole reason the two " +
			"build lines are two, and it would ship several percent of Lua the " +
			"game parses at every load for a collector nothing can ever step")
	}
	for _, want := range append(factorio.CollectorSurface(), "fk/fkgc", "OWN cargo") {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

// EITHER HEADER'S RECIPE RUNS, IN A PROJECT THAT DECLARES TWO LANGUAGES, and
// this is the half a shape assertion cannot buy.
//
// The single-language end-to-end tests above run a recipe whose data build and
// whose key are the same language by construction, so they were green over a
// block that could only be right there. In a two-language project `data_module`
// takes the FIRST language, and the OTHER language's header is the one whose
// paste has to build a module of a language its own file is not written in.
// Before the block was generated, that paste ended on `fklua mod: data module:
// open <path>: no such file or directory` -- an open error naming a wasm
// nothing in the block had built, which is the failure the block's own
// paragraph says a recipe must not produce.
//
// Four arms: both `lang` orders, and in each order both headers. The two
// key-holding arms are the control -- they were already right, and a change
// that fixed the other two by breaking these would be caught here rather than
// by nothing.
func TestEitherScaffoldedRecipeRunsInATwoLanguageProject(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this builds a guest with tinygo and one with cargo")
	}
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ langs, key string }{
		{"go,rust", "go"},
		{"rust,go", "rust"},
	} {
		for _, header := range []string{"go", "rust"} {
			t.Run(tc.langs+"/"+header, func(t *testing.T) {
				dir := t.TempDir()
				back := chdir(t, dir)
				defer back()

				const modName = "two-lang-mod"
				if err := runInit([]string{modName, "--lang", tc.langs, "--data",
					"--guest-module", root}); err != nil {
					t.Fatalf("fklua init --data: %v", err)
				}
				t.Setenv("FKLUA_API_DIR", filepath.Join(root, "api"))
				if err := runGenBindings([]string{"--lang=rust"}); err != nil {
					t.Fatalf("fklua gen-bindings, which is init's own next "+
						"step: %v", err)
				}

				path := filepath.Join(dir, goDataGuestDir, "main.go")
				if header == "rust" {
					path = filepath.Join(dir, rustDataCrateDir(modName),
						"src", "lib.rs")
				}
				// THE WHOLE RECIPE COMES OUT OF THE HEADER, as on the
				// single-language arms: nothing here builds anything by other
				// means, so a block missing a line packages an open error.
				out := filepath.Join(dir, "out")
				args := runRecipe(t, dir, path, out)
				if len(args) != 1 ||
					args[0] != controlArtifactPath(modName, header) {
					t.Fatalf("the recipe packages %v; the positional is the "+
						"%s control guest at %s", args, langName(header),
						controlArtifactPath(modName, header))
				}
				pkg := filepath.Join(out, modName+"_0.1.0")
				for _, want := range []string{"control.lua", "data.lua",
					factorio.DataStageFile, factorio.DataModuleFile} {
					if _, err := os.Stat(filepath.Join(pkg, want)); err != nil {
						t.Errorf("the packaged mod has no %s: %v", want, err)
					}
				}
			})
		}
	}
}

// printedRecipes splits `fklua init`'s own printed `Next:` block into the
// per-language build-and-package recipes, keyed by the language of the control
// guest each one packages.
//
// It exists because the SCAFFOLDED recipe and the PRINTED one are two surfaces
// over one rule, and only the scaffolded one had a reader. The block init
// prints is the first thing an author sees and the thing they paste, and it
// went on printing each language's own data build for a whole round after the
// headers stopped: pasting a two-language project's non-key block ended on
// `fklua mod: data module: open <path>: no such file or directory`.
//
// The preamble lines are dropped rather than mis-assigned: `go mod tidy` and
// `fklua gen-bindings && fklua lock` belong to the project rather than to
// either language, and a block runs from the first build line after them to
// its own `fklua mod`.
func printedRecipes(t *testing.T, out, modName string) map[string][]string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "\nNext:\n")
	if !ok {
		t.Fatalf("init printed no `Next:` block at all:\n%s", out)
	}
	var cmds []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		cmd := strings.TrimSpace(line)
		// A continuation line belongs to the command above it, exactly as in
		// recipeFrom -- the two extractors read one recipe in two renderings.
		if n := len(cmds); n > 0 && strings.HasSuffix(cmds[n-1], "\\") {
			cmds[n-1] = strings.TrimSpace(strings.TrimSuffix(cmds[n-1], "\\")) + " " + cmd
			continue
		}
		cmds = append(cmds, cmd)
	}
	recipes := map[string][]string{}
	var cur []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "fklua gen-bindings") || strings.Contains(c, "go mod tidy") {
			cur = nil
			continue
		}
		cur = append(cur, c)
		if !strings.HasPrefix(c, "fklua mod ") {
			continue
		}
		args := shellFields(t, c)
		if len(args) != 3 {
			t.Fatalf("init printed `%s`, which is not `fklua mod <one wasm>`", c)
		}
		lang := ""
		for _, l := range []string{"go", "rust"} {
			if args[2] == controlArtifactPath(modName, l) {
				lang = l
			}
		}
		if lang == "" {
			t.Fatalf("init printed `%s`, whose positional is neither control "+
				"artifact (%s, %s)", c, controlArtifactPath(modName, "go"),
				controlArtifactPath(modName, "rust"))
		}
		if _, dup := recipes[lang]; dup {
			t.Fatalf("init printed two blocks packaging the %s control guest, "+
				"so a test reading one of them is reading whichever came "+
				"last:\n%s", langName(lang), out)
		}
		recipes[lang] = cur
		cur = nil
	}
	if len(recipes) == 0 {
		t.Fatalf("no printed block ends in an `fklua mod` line, so there is "+
			"nothing here to run:\n%s", out)
	}
	return recipes
}

// INIT'S PRINTED STEPS AND THE SCAFFOLDED HEADERS ARE ONE RECIPE, asserted
// against hand-spelled command lines in both `lang` orders and needing no
// toolchain.
//
// This is the structural half of the same claim TestInitsPrintedStepsRunIn-
// ATwoLanguageProject measures by running it. Both halves are worth having:
// the run is the only thing that can say the recipe WORKS, and it skips on a
// machine without tinygo or cargo, where this one still holds the two surfaces
// to one answer.
//
// Every expected string is spelled by hand (controlLine, dataLine), so a
// generator that produced the same wrong block on both surfaces could not
// satisfy it -- which is the whole hazard, since both surfaces are now
// rendered from one function.
func TestInitsPrintedStepsAndTheScaffoldedHeadersAreOneRecipe(t *testing.T) {
	for _, tc := range []struct{ langs, key string }{
		{"go,rust", "go"},
		{"rust,go", "rust"},
	} {
		t.Run(tc.langs, func(t *testing.T) {
			dir := t.TempDir()
			back := chdir(t, dir)
			defer back()

			const modName = "printed-mod"
			out := captureStdout(t, func() error {
				return runInit([]string{modName, "--lang", tc.langs, "--data"})
			})
			printed := printedRecipes(t, out, modName)
			if len(printed) != 2 {
				t.Fatalf("a project declaring %q printed %d block(s); one per "+
					"language is what an author picks between:\n%s",
					tc.langs, len(printed), out)
			}
			for _, lang := range []string{"go", "rust"} {
				// THE DATA LINE IS THE KEY'S, whatever language this block's
				// control guest is: `data_module` names one module and the
				// packaging line reads that one.
				want := []string{
					controlLine(modName, lang),
					dataLine(modName, tc.key),
					"fklua mod " + controlArtifactPath(modName, lang),
				}
				got := printed[lang]
				if len(got) != len(want) {
					t.Fatalf("init's printed %s block is %v, want %v",
						langName(lang), got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("init's printed %s block, line %d, is\n  %s\n"+
							"want\n  %s\n(line 2 builds the module "+
							"`data_module` names, which `lang = %q` gives to "+
							"%s; a literal paste that builds this block's own "+
							"language instead ends on an open error naming a "+
							"wasm nothing in the block ever built)",
							langName(lang), i+1, got[i], want[i], tc.langs,
							langName(tc.key))
					}
				}
				// AND IT IS THE HEADER'S BLOCK, character for character. The
				// two surfaces are the thing that drifted.
				path := filepath.Join(dir, goDataGuestDir, "main.go")
				if lang == "rust" {
					path = filepath.Join(dir, rustDataCrateDir(modName),
						"src", "lib.rs")
				}
				header := recipeFrom(t, path)
				if len(header) != len(got) {
					t.Fatalf("%s carries %v and init prints %v", path, header, got)
				}
				for i := range got {
					if header[i] != got[i] {
						t.Errorf("line %d differs between what init PRINTS and "+
							"what %s carries:\n  printed: %s\n  header:  %s",
							i+1, path, got[i], header[i])
					}
				}
			}
		})
	}
}

// INIT'S OWN PRINTED STEPS RUN, IN A PROJECT THAT DECLARES TWO LANGUAGES, in
// both `lang` orders and for both blocks.
//
// The scaffolded headers got this test a round before the printed steps did,
// and the printed steps are the surface an author actually pastes from. Both
// blocks are run, because the non-key one is the case that failed and the key
// one is the control that was already right.
//
// EACH BLOCK RUNS AGAINST A CLEARED SET OF ARTIFACTS, which is the tooth: the
// two blocks share one project so the toolchain caches stay warm, and without
// the clearing the second block would package whatever the first had left on
// disk -- a recipe measured against a stale artifact is a recipe not measured
// at all.
func TestInitsPrintedStepsRunInATwoLanguageProject(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this builds a guest with tinygo and one with cargo")
	}
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ langs, key string }{
		{"go,rust", "go"},
		{"rust,go", "rust"},
	} {
		t.Run(tc.langs, func(t *testing.T) {
			dir := t.TempDir()
			back := chdir(t, dir)
			defer back()

			const modName = "printed-run-mod"
			out := captureStdout(t, func() error {
				return runInit([]string{modName, "--lang", tc.langs, "--data",
					"--guest-module", root})
			})
			printed := printedRecipes(t, out, modName)
			t.Setenv("FKLUA_API_DIR", filepath.Join(root, "api"))
			if err := runGenBindings([]string{"--lang=rust"}); err != nil {
				t.Fatalf("fklua gen-bindings, which is init's own next step: %v", err)
			}
			for _, lang := range []string{"go", "rust"} {
				cmds := printed[lang]
				if len(cmds) != 3 || !strings.HasPrefix(cmds[2], "fklua mod") {
					t.Fatalf("init's printed %s block is %v; it must build the "+
						"control guest, build the data module and package "+
						"them, in that order", langName(lang), cmds)
				}
				// Every wasm this project can produce, gone before the block
				// runs. Cargo and tinygo keep their own caches elsewhere, so
				// this costs a relink and buys a block that cannot pass on
				// the other block's output.
				for _, w := range []string{
					GoGuestArtifact(modName), GoDataArtifact(modName),
					rustReleaseWasm(RustGuestArtifact(modName)),
					rustReleaseWasm(RustDataArtifact(modName)),
				} {
					if err := os.Remove(filepath.Join(dir, w)); err != nil &&
						!os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
				o := filepath.Join(dir, "out-"+lang)
				args := runCommands(t, dir, "init's printed "+langName(lang)+
					" block", cmds, o)
				if len(args) != 1 || args[0] != controlArtifactPath(modName, lang) {
					t.Fatalf("the block packages %v; the positional is the %s "+
						"control guest at %s", args, langName(lang),
						controlArtifactPath(modName, lang))
				}
				pkg := filepath.Join(o, modName+"_0.1.0")
				for _, want := range []string{"control.lua", "data.lua",
					factorio.DataStageFile, factorio.DataModuleFile} {
					if _, err := os.Stat(filepath.Join(pkg, want)); err != nil {
						t.Errorf("the mod packaged by init's printed %s block "+
							"has no %s: %v", langName(lang), want, err)
					}
				}
			}
		})
	}
}
