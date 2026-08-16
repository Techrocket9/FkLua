// Package luahost runs Lua scripts under lua52f, the Factorio-shaped Lua 5.2.1
// interpreter built by third_party/lua-5.2.1/fetch.sh.
//
// Timing is measured from outside the process on purpose. Factorio's sandbox has
// no os.clock -- the only clock in game is helpers.create_profiler(), which does
// not exist here -- so lua52f faithfully has no clock either. Adding one would
// make the oracle stop matching the thing it is meant to model.
package luahost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// ErrNotBuilt reports that lua52f is missing. It is not something to work around
// by falling back to a system Lua: Homebrew's `lua` is 5.5, whose integer subtype
// makes %, overflow and string.pack behave differently from Factorio's 5.2.1, so
// a fallback would silently produce numbers that do not describe the game.
//
// THE MESSAGE NAMES THE WORKTREE CASE FIRST, and that is not politeness. `/bin/`
// is gitignored, so EVERY fresh `git worktree add` starts without the oracle,
// and about thirty tests across five packages -- the whole collector suite among
// them -- respond by SKIPPING. A skipped test prints nothing without `-v`, so
// `go test ./...` says `ok` for a run that checked nothing. Stage D found the
// collector suite in exactly that state, and it read as a pass.
//
// Copying is the remedy to lead with because `make lua52f` in a worktree would
// otherwise re-fetch and rebuild Lua from source, which is minutes, to produce
// the binary the main checkout already has. `make lua52f` now does that copy
// itself when there is a main checkout to copy from.
var ErrNotBuilt = errors.New(
	"lua52f not built. In a git worktree: run `make lua52f` (it copies the " +
		"binary from the main checkout), or copy bin/lua52f across by hand. " +
		"In a fresh clone `make lua52f` builds it from third_party/lua-5.2.1. " +
		"Without it every host-side test SKIPS, and a skipped test reads " +
		"exactly like a pass")

// Host runs scripts under a specific lua52f binary.
type Host struct {
	Bin string
}

// Find locates bin/lua52f by walking up from the working directory until it finds
// a go.mod, so it works from any package's test directory.
func Find() (*Host, error) {
	if env := os.Getenv("LUA52F"); env != "" {
		if _, err := os.Stat(env); err != nil {
			return nil, fmt.Errorf("LUA52F=%s: %w", env, err)
		}
		return &Host{Bin: env}, nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			bin := filepath.Join(dir, "bin", "lua52f")
			if _, err := os.Stat(bin); err != nil {
				return nil, ErrNotBuilt
			}
			return &Host{Bin: bin}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errors.New("luahost: no go.mod found above the working directory")
		}
		dir = parent
	}
}

// Run executes a script file and returns its stdout.
func (h *Host) Run(script string, args ...string) (string, error) {
	out, _, err := h.run(script, args...)
	return out, err
}

// RunString executes Lua source directly, via a temporary file. lua52f cannot
// take a program on stdin and also receive script arguments, so a file it is.
func (h *Host) RunString(src string, args ...string) (string, error) {
	f, err := os.CreateTemp("", "fklua-*.lua")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(src); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return h.Run(f.Name(), args...)
}

func (h *Host) run(script string, args ...string) (string, time.Duration, error) {
	cmd := exec.Command(h.Bin, append([]string{script}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		return stdout.String(), elapsed, fmt.Errorf("lua52f %s: %w\n%s",
			filepath.Base(script), err, stderr.String())
	}
	return stdout.String(), elapsed, nil
}

// Timing is the result of a timed measurement.
type Timing struct {
	// Median wall time with the interpreter's startup cost removed. Median
	// rather than mean because an occasional scheduler hiccup should not move
	// the number, and minimum would hide real variance.
	Elapsed time.Duration
	Runs    []time.Duration
	Startup time.Duration
}

// NsPerOp divides the measured time by an operation count.
func (t Timing) NsPerOp(ops int64) float64 {
	if ops <= 0 {
		return 0
	}
	return float64(t.Elapsed.Nanoseconds()) / float64(ops)
}

// Time runs a script `runs` times and reports the median duration net of process
// startup. Startup is measured with an empty script and subtracted, so kernels
// need to run long enough for that subtraction to be small relative to the
// signal -- aim for a hundred milliseconds or more per run.
func (h *Host) Time(script string, runs int, args ...string) (Timing, error) {
	if runs < 1 {
		runs = 1
	}

	startup, err := h.measureStartup()
	if err != nil {
		return Timing{}, err
	}

	// One untimed pass so any first-touch page faults and filesystem cache
	// misses land outside the measurement.
	if _, _, err := h.run(script, args...); err != nil {
		return Timing{}, err
	}

	durations := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		_, d, err := h.run(script, args...)
		if err != nil {
			return Timing{}, err
		}
		durations = append(durations, d)
	}

	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	median := sorted[len(sorted)/2]

	net := median - startup
	if net < 0 {
		net = 0
	}
	return Timing{Elapsed: net, Runs: durations, Startup: startup}, nil
}

// measureStartup times an empty script, so process spawn and interpreter setup
// can be subtracted from kernel measurements.
func (h *Host) measureStartup() (time.Duration, error) {
	f, err := os.CreateTemp("", "fklua-empty-*.lua")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("return 0\n"); err != nil {
		f.Close()
		return 0, err
	}
	f.Close()

	const n = 7
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		_, d, err := h.run(f.Name())
		if err != nil {
			return 0, err
		}
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], nil
}

// Available reports whether lua52f can be found, for skipping tests with a
// useful message rather than a confusing failure.
func Available() (bool, string) {
	h, err := Find()
	if err != nil {
		return false, err.Error()
	}
	if runtime.GOOS == "windows" {
		return false, "lua52f is not built on windows"
	}
	return true, h.Bin
}
