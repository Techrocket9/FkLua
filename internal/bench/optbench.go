package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Techrocket9/fklua/internal/luahost"
)

// OptKernel is one wasm module measured at several optimization levels.
//
// It exists because the M0 kernels cannot answer the question M5 asks. Those
// are hand-written Lua standing in for what the emitter produces, which is what
// makes them a CEILING -- they do not move when the emitter improves, and a
// pass that halves the work in a generated loop shows up in them as exactly
// nothing. These compile a real module with the real compiler and time the
// result, so the number moves when the compiler does.
type OptKernel struct {
	Name string
	// File is a .wat under bench/wasm; Driver is the .lua next to it holding
	// the body that exercises the instantiated module, bound as M.
	File   string
	Driver string
	Why    string
}

// OptKernels mirrors the M0 set, so a ratio here and a ratio there are talking
// about the same program.
var OptKernels = []OptKernel{
	{Name: "sum", File: "sum.wat", Driver: "sum.lua",
		Why: "u32 array sum through linear memory"},
	{Name: "chase", File: "chase.wat", Driver: "chase.lua",
		Why: "pointer chase over structs -- loads whose address is a load"},
	{Name: "prng", File: "prng.wat", Driver: "prng.lua",
		Why: "xorshift32 -- shifts, xor and wrapping, no memory at all"},
	{Name: "dot", File: "dot.wat", Driver: "dot.lua",
		Why: "f64 dot product -- every element reassembles a double from two words"},
	{Name: "fib", File: "fib.wat", Driver: "fib.lua",
		Why: "recursive fib(30) -- call dispatch and little else"},
	{Name: "frame", File: "frame.wat", Driver: "frame.lua",
		Why: "f64 through a shadow-stack frame -- what typed-slot promotion removes"},
	{Name: "count", File: "count.wat", Driver: "count.lua",
		Why: "the canonical counted loop -- what the loop-header fixpoint removes"},
	{Name: "constdiv", File: "constdiv.wat", Driver: "constdiv.lua",
		Why: "div/rem by a constant -- what the constant-divisor lowering removes"},
}

// Compiler renders one .wat at one optimization level.
type Compiler func(watPath string, level int) (string, error)

// RunOpt compiles each kernel at each level and times the result.
//
// The generated chunk is wrapped in a vararg function rather than loaded as a
// chunk of its own: lua52f has no loadfile and no io, so everything that runs
// has to be one source file -- which is also how a Factorio mod ships.
func RunOpt(h *luahost.Host, dir string, kernels []OptKernel, levels []int, runs int, compile Compiler) ([]Result, error) {
	var out []Result

	for _, k := range kernels {
		driver, err := os.ReadFile(filepath.Join(dir, k.Driver))
		if err != nil {
			return nil, err
		}

		byLevel := map[int]*Result{}
		for _, lvl := range levels {
			src, err := compile(filepath.Join(dir, k.File), lvl)
			if err != nil {
				return nil, fmt.Errorf("%s at -opt=%d: %w", k.Name, lvl, err)
			}

			var b strings.Builder
			b.WriteString("local M = (function(...)\n")
			b.WriteString(src)
			b.WriteString("\nend)()\n")
			b.Write(driver)
			script := b.String()

			path := filepath.Join(os.TempDir(),
				fmt.Sprintf("fklua-opt-%s-%d.lua", k.Name, lvl))
			if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
				return nil, err
			}

			// One untimed run for the checksum, which must agree across levels:
			// an optimization that computes something else is not an
			// optimization, and the M0 gate makes the same demand of variants.
			stdout, err := h.Run(path)
			if err != nil {
				os.Remove(path)
				return nil, fmt.Errorf("%s at -opt=%d: %w", k.Name, lvl, err)
			}
			checksum, ops, err := parseOutput(stdout)
			if err != nil {
				os.Remove(path)
				return nil, fmt.Errorf("%s at -opt=%d: %w", k.Name, lvl, err)
			}

			t, err := h.Time(path, runs)
			os.Remove(path)
			if err != nil {
				return nil, fmt.Errorf("%s at -opt=%d: %w", k.Name, lvl, err)
			}

			r := Result{
				Kernel:   k.Name,
				Variant:  fmt.Sprintf("opt%d", lvl),
				Ops:      ops,
				Checksum: checksum,
				NsPerOp:  t.NsPerOp(ops),
			}
			byLevel[lvl] = &r
			out = append(out, r)
		}

		// Ratios are against -opt=0, so a number below 1.00 is a speedup.
		base := byLevel[levels[0]]
		for i := range out {
			if out[i].Kernel == k.Name && base != nil && base.NsPerOp > 0 {
				out[i].Ratio = out[i].NsPerOp / base.NsPerOp
			}
		}
	}
	return out, nil
}

// ReportOpt prints the level-by-level table.
func ReportOpt(results []Result, kernels []OptKernel) string {
	byKernel := map[string][]Result{}
	for _, r := range results {
		byKernel[r.Kernel] = append(byKernel[r.Kernel], r)
	}
	var b strings.Builder
	for _, k := range kernels {
		rs := byKernel[k.Name]
		if len(rs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\n    %s\n", k.Name, k.Why)
		sort.Slice(rs, func(i, j int) bool { return rs[i].Variant < rs[j].Variant })
		for _, r := range rs {
			ratio := "   (baseline)"
			if r.Variant != "opt0" {
				ratio = fmt.Sprintf("  %.2fx", r.Ratio)
			}
			fmt.Fprintf(&b, "    %-9s %9.2f ns/op  %12s   %s ops\n",
				r.Variant, r.NsPerOp, ratio, humanInt(r.Ops))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// humanInt groups an op count with commas.
func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}
