package factorio

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	luart "github.com/Techrocket9/fklua/runtime"
)

// THE REGISTER KINDS ARE SPELLED IN FOUR PLACES AND HAVE TO AGREE.
//
// fk.register(kind, descp) dispatches on a small integer, and that integer is
// written down in runtime/lua/fk_mod.lua (the reader), in gogen.go's preamble
// and in rustgen_rt.go's (the two writers). Nothing connects them: a kind added
// to one and not to the rest does not fail to compile, it dispatches a
// descriptor the host reads as a DIFFERENT kind -- a remote interface's
// {name, methods} arriving where a command's {name, help, id} is expected, whose
// symptom is ERR_BAD_ARGS at load with nothing saying which end is wrong.
//
// This is factorio.Hooks' mirror one seam over, and it is checked the way that
// one had to learn to be: in every direction, over the real sources, so that
// neither "the host grew a kind" nor "a guest grew a kind" can be the silent
// half. The 2026-07-30 audit's own lesson -- a mirror checked in one direction
// drifts in the other -- is why this reads all four rather than comparing Go
// against Lua and stopping.
func TestTheRegisterKindsAgreeEverywhereTheyAreSpelled(t *testing.T) {
	// The Lua reader: `local REG_A, REG_B, REG_C = 1, 2, 3`.
	lua := luart.ModGlue()
	line := regexp.MustCompile(`(?m)^local (REG_[A-Z_, ]+) = ([0-9, ]+)$`).
		FindStringSubmatch(lua)
	if line == nil {
		t.Fatal("fk_mod.lua no longer declares the REG_ kinds in one statement; " +
			"this gate reads that statement, so either restore the shape or teach " +
			"it the new one -- silently reading nothing is how the four spellings " +
			"drift apart")
	}
	host := map[string]int{}
	names := strings.Split(line[1], ",")
	vals := strings.Split(line[2], ",")
	if len(names) != len(vals) {
		t.Fatalf("fk_mod.lua declares %d kind names and %d values", len(names),
			len(vals))
	}
	for i := range names {
		v, err := strconv.Atoi(strings.TrimSpace(vals[i]))
		if err != nil {
			t.Fatalf("fk_mod.lua kind value %q: %v", vals[i], err)
		}
		host[normalizeKind(names[i])] = v
	}
	if len(host) < 2 {
		t.Fatalf("fk_mod.lua declares %d register kinds, which is fewer than the "+
			"command and interface this seam shipped with -- this gate audited "+
			"nothing", len(host))
	}

	// The two writers, read out of the generator sources rather than out of the
	// committed bindings: the bindings are one pin's output and these constants
	// are in the PREAMBLE, so the source is where a divergence is introduced.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own directory")
	}
	dir := filepath.Dir(thisFile)
	goKinds := kindsFromSource(t, filepath.Join(dir, "gogen.go"),
		`(?m)^\s*(reg[A-Za-z]+)\s*=\s*([0-9]+)$`)
	rsKinds := kindsFromSource(t, filepath.Join(dir, "rustgen_rt.go"),
		`(?m)^const (REG_[A-Z_]+): u32 = ([0-9]+);$`)

	for _, w := range []struct {
		what  string
		kinds map[string]int
	}{{"the Go preamble", goKinds}, {"the Rust preamble", rsKinds}} {
		for name, v := range w.kinds {
			hv, known := host[name]
			if !known {
				t.Errorf("%s declares register kind %q = %d and fk_mod.lua has no "+
					"such kind: the host would fall through to ERR_BAD_ARGS with "+
					"nothing naming the cause", w.what, name, v)
				continue
			}
			if hv != v {
				t.Errorf("%s says register kind %q is %d and fk_mod.lua says %d: a "+
					"descriptor of one kind would be read as another", w.what, name,
					v, hv)
			}
		}
		for name, hv := range host {
			if _, known := w.kinds[name]; !known {
				t.Errorf("fk_mod.lua declares register kind %q = %d and %s does not, "+
					"so no guest in that language can reach it", name, hv, w.what)
			}
		}
	}
}

// normalizeKind folds the four spellings onto one key: REG_MODEVENT,
// REG_MOD_EVENT and regModEvent are the same kind wearing each language's
// casing. Comparing the raw spellings would make the gate fail on a rename that
// changed nothing, which is the kind of gate people delete.
func normalizeKind(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "REG_")
	s = strings.TrimPrefix(s, "reg")
	var b strings.Builder
	for _, r := range s {
		if r == '_' {
			continue
		}
		if r >= 'a' && r <= 'z' {
			r = r - 'a' + 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func kindsFromSource(t *testing.T, path, pat string) map[string]int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, m := range regexp.MustCompile(pat).FindAllStringSubmatch(string(b), -1) {
		v, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("%s: kind value %q: %v", path, m[2], err)
		}
		out[normalizeKind(m[1])] = v
	}
	if len(out) == 0 {
		t.Fatalf("%s: found no register-kind constants, so this gate audited "+
			"nothing on that side", path)
	}
	return out
}
