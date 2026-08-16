package luahost

import (
	"os"
	"strings"
	"testing"
)

func host(t *testing.T) *Host {
	t.Helper()
	h, err := Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	return h
}

func TestRunString(t *testing.T) {
	h := host(t)
	out, err := h.RunString(`print(21 * 2)`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "42" {
		t.Fatalf("got %q, want 42", strings.TrimSpace(out))
	}
}

func TestScriptArgs(t *testing.T) {
	h := host(t)
	out, err := h.RunString(`print(... )`, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Fatalf("got %q, want hello", strings.TrimSpace(out))
	}
}

func TestErrorIsReported(t *testing.T) {
	h := host(t)
	_, err := h.RunString(`error("boom")`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should carry the Lua message, got: %v", err)
	}
}

// The whole point of lua52f is that it is Factorio-shaped. If the binary on the
// path is a stock Lua, every measurement taken through this package is
// meaningless -- so assert the shape here rather than trusting the build.
func TestHostIsFactorioShaped(t *testing.T) {
	h := host(t)
	out, err := h.RunString(`
		print(_VERSION)
		print(coroutine == nil, io == nil, os == nil)
		print(math.type == nil)
		print(load(string.dump(function() end)) == nil)
	`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %q", len(lines), out)
	}
	if lines[0] != "Lua 5.2" {
		t.Errorf("_VERSION = %q, want Lua 5.2", lines[0])
	}
	if lines[1] != "true\ttrue\ttrue" {
		t.Errorf("coroutine/io/os should all be absent, got %q", lines[1])
	}
	if lines[2] != "true" {
		t.Error("math.type should be absent (no integer subtype)")
	}
	if lines[3] != "true" {
		t.Error("load() should reject binary chunks")
	}
}

func TestTimeSubtractsStartup(t *testing.T) {
	h := host(t)
	f := t.TempDir() + "/spin.lua"
	src := `local s = 0 for i = 1, 3000000 do s = s + i end print(s)`
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	tm, err := h.Time(f, 3)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Startup <= 0 {
		t.Error("startup should be measured as positive")
	}
	if tm.Elapsed <= 0 {
		t.Fatalf("elapsed should be positive, got %v", tm.Elapsed)
	}
	// The loop must dominate startup, otherwise the subtraction is noise and
	// every kernel number built on it is untrustworthy.
	if tm.Elapsed < tm.Startup {
		t.Errorf("work (%v) should exceed startup (%v); the kernel is too short to measure",
			tm.Elapsed, tm.Startup)
	}
	if len(tm.Runs) != 3 {
		t.Errorf("expected 3 runs, got %d", len(tm.Runs))
	}
}

func TestNsPerOp(t *testing.T) {
	tm := Timing{Elapsed: 1e9} // 1 second
	if got := tm.NsPerOp(1e6); got != 1000 {
		t.Errorf("NsPerOp = %v, want 1000", got)
	}
	if got := tm.NsPerOp(0); got != 0 {
		t.Errorf("NsPerOp(0) = %v, want 0 (no division by zero)", got)
	}
	if got := tm.NsPerOp(-5); got != 0 {
		t.Errorf("NsPerOp(-5) = %v, want 0", got)
	}
}
