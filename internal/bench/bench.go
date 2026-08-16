// Package bench runs the M0 kernels and compares FkLua-style generated Lua
// against the Lua a mod author would write by hand.
//
// The ratio between those two is the project's whole justification. If generated
// code is far enough behind hand-written Lua, a mod author is better off writing
// Lua and there is nothing worth building here -- so the thresholds below are a
// genuine go/no-go, not decoration.
package bench

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Techrocket9/fklua/internal/luahost"
)

// Kernel is one benchmark: a Lua file run under several variants.
type Kernel struct {
	Name string
	File string
	Args []string

	// Baseline is the variant representing hand-written Lua; Variants are
	// measured against it.
	Baseline string
	Variants []string

	// MaxRatio is the gate. Zero means the kernel is informational only.
	MaxRatio float64

	// Why explains what the kernel is actually testing.
	Why string
}

// M0Kernels is the milestone-0 set. sum and chase carry the overall go/no-go.
var M0Kernels = []Kernel{
	{
		Name:     "sum",
		File:     "sum.lua",
		Baseline: "nat",
		Variants: []string{"gen"},
		MaxRatio: 15,
		Why:      "u32 array sum through linear memory -- the primary gate",
	},
	{
		Name:     "chase",
		File:     "chase.lua",
		Baseline: "nat",
		Variants: []string{"gen"},
		MaxRatio: 15,
		Why:      "pointer chase over structs -- the primary gate",
	},
	{
		Name:     "prng",
		File:     "prng.lua",
		Baseline: "nat",
		Variants: []string{"gen"},
		MaxRatio: 6,
		Why:      "xorshift32 -- shifts and xor; worst case for arithmetic-over-bit32",
	},
	{
		Name:     "dot",
		File:     "dot.lua",
		Baseline: "nat",
		Variants: []string{"gen", "genslot"},
		Why:      "f64 dot product -- gen vs genslot sizes the typed-slot promotion pass",
	},
	{
		Name:     "fib",
		File:     "fib.lua",
		Baseline: "nat",
		Variants: []string{"gen", "genup"},
		Why:      "recursive fib(30) -- gen vs genup sizes upvalue promotion",
	},
}

// Result is one variant's measurement.
type Result struct {
	Kernel   string
	Variant  string
	Ops      int64
	Checksum string
	NsPerOp  float64
	Ratio    float64 // versus the kernel's baseline variant
}

// Run measures every variant of every kernel. runs is the repeat count per
// measurement; the median is taken.
func Run(h *luahost.Host, dir string, kernels []Kernel, runs int) ([]Result, error) {
	var out []Result

	for _, k := range kernels {
		path := filepath.Join(dir, k.File)

		all := append([]string{k.Baseline}, k.Variants...)
		byVariant := map[string]*Result{}

		for _, v := range all {
			args := append([]string{v}, k.Args...)

			// Run once untimed to capture the checksum, which must match across
			// variants -- a faster variant that computes something else is not a
			// faster variant.
			stdout, err := h.Run(path, args...)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", k.Name, v, err)
			}
			checksum, ops, err := parseOutput(stdout)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", k.Name, v, err)
			}

			t, err := h.Time(path, runs, args...)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", k.Name, v, err)
			}

			r := Result{
				Kernel:   k.Name,
				Variant:  v,
				Ops:      ops,
				Checksum: checksum,
				NsPerOp:  t.NsPerOp(ops),
			}
			byVariant[v] = &r
			out = append(out, r)
		}

		base := byVariant[k.Baseline]
		for i := range out {
			if out[i].Kernel == k.Name && base.NsPerOp > 0 {
				out[i].Ratio = out[i].NsPerOp / base.NsPerOp
			}
		}
	}
	return out, nil
}

// CheckChecksums reports variants of the same kernel that disagree on the
// answer. Applies only to kernels whose variants are meant to be equivalent.
func CheckChecksums(results []Result) []string {
	seen := map[string]string{}
	var bad []string
	for _, r := range results {
		if prev, ok := seen[r.Kernel]; ok {
			if prev != r.Checksum {
				bad = append(bad, fmt.Sprintf(
					"%s: variant %s produced %s, expected %s -- variants must compute the same thing",
					r.Kernel, r.Variant, r.Checksum, prev))
			}
		} else {
			seen[r.Kernel] = r.Checksum
		}
	}
	return bad
}

// Gate applies each kernel's MaxRatio and reports the failures.
func Gate(results []Result, kernels []Kernel) []string {
	limits := map[string]float64{}
	for _, k := range kernels {
		if k.MaxRatio > 0 {
			limits[k.Name] = k.MaxRatio
		}
	}
	var fails []string
	for _, r := range results {
		lim, ok := limits[r.Kernel]
		if !ok || r.Variant == "nat" {
			continue
		}
		if r.Ratio > lim {
			fails = append(fails, fmt.Sprintf(
				"%s/%s: %.1fx hand-written Lua, over the %.0fx limit",
				r.Kernel, r.Variant, r.Ratio, lim))
		}
	}
	return fails
}

func parseOutput(s string) (checksum string, ops int64, err error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("expected `checksum<TAB>ops`, got %q", strings.TrimSpace(s))
	}
	ops, err = strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("bad op count %q: %w", fields[1], err)
	}
	if ops <= 0 {
		return "", 0, fmt.Errorf("op count must be positive, got %d", ops)
	}
	return fields[0], ops, nil
}
