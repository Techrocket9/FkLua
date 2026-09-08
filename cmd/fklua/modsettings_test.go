package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/modsettings"
)

// `fklua modsettings` is the only thing in this toolchain that writes a file
// the ENGINE reads rather than one Factorio loads as Lua, so what these hold is
// the two ends of that: the bytes survive a trip out and back through the
// command, and every refusal names the byte or the key rather than leaving the
// engine to say "could not read mod-settings.dat".

// msWrite runs `modsettings write` and returns the path it wrote.
func msWrite(t *testing.T, dir, jsonText string) string {
	t.Helper()
	from := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(from, []byte(jsonText), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "mod-settings.dat")
	captureStdout(t, func() error {
		return runModSettingsWrite([]string{"--from", from, "--out", out})
	})
	return out
}

// THE ROUND TRIP THROUGH THE COMMAND, which is a different claim from the
// package's own: it says the two halves of the CLI agree, so a gate that writes
// a file and later reads the engine's rewrite of it is reading with the same
// understanding it wrote with.
func TestModSettingsWritesAFileItsOwnReadGivesBack(t *testing.T) {
	dir := t.TempDir()
	source := `{
	  "comment": "read and ignored",
	  "version": [2, 0, 77, 0],
	  "startup": {"a-bool": false, "an-int": 40, "a-double": 20.0, "a-text": "1 steel-plate, 10 water"},
	  "runtime-global": {},
	  "runtime-per-user": {}
	}`
	out := msWrite(t, dir, source)

	printed := captureStdout(t, func() error { return runModSettingsRead([]string{out}) })
	for _, want := range []string{
		`"version": [2, 0, 77, 0]`,
		`"a-bool": false`,
		`"an-int": 40`,
		`"a-double": 20.0`,
		`"a-text": "1 steel-plate, 10 water"`,
		`"runtime-global": {}`,
		`"runtime-per-user": {}`,
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("read did not print %s:\n%s", want, printed)
		}
	}
	// A comment reaches no byte, so it cannot come back out.
	if strings.Contains(printed, "read and ignored") {
		t.Errorf("the comment reached the file:\n%s", printed)
	}

	// And what it printed is what `write` reads: the same bytes come back from
	// a second trip through both halves.
	second := msWrite(t, t.TempDir(), printed)
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Errorf("what `read` printed did not write back to the same file (%d bytes, then %d)",
			len(first), len(again))
	}
}

// WRITE SAYS HOW MANY BYTES IT WROTE AND WHERE. The gate prints this line, and
// a writer that silently wrote nothing is the failure it makes visible.
func TestModSettingsWriteReportsWhatItWrote(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(from, []byte(`{"version":[2,0,77,0],"startup":{"a":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "mod-settings.dat")
	printed := captureStdout(t, func() error {
		return runModSettingsWrite([]string{"--from", from, "--out", out})
	})
	if !strings.Contains(printed, "wrote ") || !strings.Contains(printed, out) {
		t.Errorf("write said %q, and it owes the reader a byte count and a path", printed)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("write printed a line and wrote no file: %v", err)
	}
	if !strings.Contains(printed, "wrote "+itoa(int(info.Size()))+" bytes") {
		t.Errorf("write reported a different size from the one on disk (%d): %q", info.Size(), printed)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// EVERY REFUSAL NAMES THE BYTE OR THE KEY. The engine's own diagnosis for a
// file it cannot read names the FILE, which is no help at all when the question
// is which row was written wrong.
func TestModSettingsRefusalsNameTheByteOrTheKey(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// A real file, to truncate.
	good := msWrite(t, t.TempDir(), `{"version":[2,0,77,0],"startup":{"a-setting":"a value"}}`)
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated.dat")
	if err := os.WriteFile(truncated, raw[:len(raw)-3], 0o644); err != nil {
		t.Fatal(err)
	}
	trailing := filepath.Join(dir, "trailing.dat")
	if err := os.WriteFile(trailing, append(append([]byte{}, raw...), 0xff), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dat holding a byte string that is not UTF-8. It cannot be written with
	// `write`, because JSON cannot say it; it is built here the way anything
	// else would produce one, and the claim is that READ REFUSES IT rather than
	// printing a U+FFFD the next `write` would turn into different bytes.
	notUTF8 := filepath.Join(dir, "not-utf8.dat")
	if err := os.WriteFile(notUTF8, modsettings.Encode(modsettings.Document{
		Version: [4]uint16{2, 0, 77, 0},
		Root: modsettings.Dict(
			modsettings.Pair("startup", modsettings.Dict(
				modsettings.Pair("a-setting", modsettings.Dict(
					modsettings.Pair("value", modsettings.Str("caf\xe9 latte")))))),
			modsettings.Pair("runtime-global", modsettings.Dict()),
			modsettings.Pair("runtime-per-user", modsettings.Dict()),
		),
	}), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "a JSON row naming a section nothing knows",
			run: func() error {
				return runModSettingsWrite([]string{
					"--from", writeFile("bad-section.json", `{"version":[2,0,77,0],"data":{}}`),
					"--out", filepath.Join(dir, "never.dat")})
			},
			want: `names "data"`,
		},
		{
			name: "a JSON row whose setting holds a list",
			run: func() error {
				return runModSettingsWrite([]string{
					"--from", writeFile("bad-value.json", `{"version":[2,0,77,0],"startup":{"a-setting":[1]}}`),
					"--out", filepath.Join(dir, "never.dat")})
			},
			want: "a-setting holds a list",
		},
		{
			name: "a JSON file that is not there",
			run: func() error {
				return runModSettingsWrite([]string{
					"--from", filepath.Join(dir, "absent.json"),
					"--out", filepath.Join(dir, "never.dat")})
			},
			want: "absent.json",
		},
		{
			name: "write with no --out",
			run:  func() error { return runModSettingsWrite([]string{"--from", "x.json"}) },
			want: "usage: fklua modsettings write",
		},
		{
			name: "write with an argument it does not know",
			run: func() error {
				return runModSettingsWrite([]string{"--from", "x.json", "--out", "y.dat", "--force"})
			},
			want: `unknown argument "--force"`,
		},
		{
			name: "a dat that stops in the middle of a value",
			run:  func() error { return runModSettingsRead([]string{truncated}) },
			want: "ends in the middle of",
		},
		{
			name: "a dat with a byte after the root node",
			run:  func() error { return runModSettingsRead([]string{trailing}) },
			want: "1 byte is left over after the root node, at offset",
		},
		{
			name: "a dat holding bytes that are not UTF-8",
			run:  func() error { return runModSettingsRead([]string{notUTF8}) },
			want: "the value of startup.a-setting holds bytes that are not UTF-8",
		},
		{
			name: "read with no path",
			run:  func() error { return runModSettingsRead(nil) },
			want: "usage: fklua modsettings read",
		},
		{
			name: "read given two paths",
			run:  func() error { return runModSettingsRead([]string{"a.dat", "b.dat"}) },
			want: "read takes one mod-settings.dat",
		},
		{
			name: "read given a flag",
			run:  func() error { return runModSettingsRead([]string{"--json", "a.dat"}) },
			want: `unknown flag "--json"`,
		},
		{
			name: "no subcommand at all",
			run:  func() error { return runModSettings(nil) },
			want: "usage: fklua modsettings write",
		},
		{
			name: "a subcommand nothing knows",
			run:  func() error { return runModSettings([]string{"edit"}) },
			want: `unknown modsettings subcommand "edit"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run()
			if err == nil {
				t.Fatalf("it was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say why\n got: %s\nwant it to contain: %s", err, c.want)
			}
		})
	}
}

// A REFUSED WRITE LEAVES NO FILE. A gate that copied a half-written or stale
// mod-settings.dat into a mod directory would run the engine on the wrong
// values and report whatever came out.
func TestARefusedWriteWritesNothing(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(from, []byte(`{"version":[2,0,77,0],"startup":{"a":null}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "mod-settings.dat")
	if err := runModSettingsWrite([]string{"--from", from, "--out", out}); err == nil {
		t.Fatalf("a null value was accepted")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused write left %s behind", out)
	}
}

// THE COMMAND IS REACHABLE, and that is a separate claim from "the functions
// work": everything above calls runModSettings* directly, so a subcommand
// missing from main's switch would leave every test here green and `fklua
// modsettings` printing the usage text. Same shape as the two commands that
// once disagreed about one manifest key.
func TestTheModSettingsCommandIsWiredAndDocumented(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	main := readFile(t, filepath.Join(root, "cmd", "fklua", "main.go"))
	if !strings.Contains(main, `case "modsettings":`) {
		t.Errorf("main's switch has no modsettings case, so the command cannot be reached")
	}
	if !strings.Contains(main, "runModSettings(os.Args[2:])") {
		t.Errorf("main's modsettings case does not call runModSettings")
	}
	for _, want := range []string{
		"fklua modsettings write --from FILE.json --out mod-settings.dat",
		"fklua modsettings read mod-settings.dat",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("`fklua --help` does not name %q", want)
		}
	}
}
