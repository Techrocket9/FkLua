package luagen

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luahost"
	luart "github.com/Techrocket9/fklua/runtime"
)

// f32 rounding is load-bearing: every f32 operation must round its result to
// single precision or results drift from the spec. The implementation is
// arithmetic (Dekker split) rather than string.pack-based, because string.pack
// allocates per operation, so it has to be validated rather than trusted.
//
// Go's own float64->float32 conversion is the oracle.
func TestF32RoundingMatchesGo(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	var vals []float64
	add := func(v float64) { vals = append(vals, v) }

	// Exactly representable, and values needing a round.
	for _, v := range []float64{
		0, 1, -1, 0.5, -0.5, 2, 3, 3.14159265358979, -3.14159265358979,
		1e-10, 1e10, 1e30, -1e30, 16777216, 16777217, 16777215,
		1.0 / 3.0, 2.0 / 3.0, math.Pi, math.E, math.Sqrt2,
	} {
		add(v)
	}
	// Boundaries: max finite f32, the overflow threshold, min normal, subnormals.
	add(3.4028234663852886e38) // FLT_MAX
	add(3.4028235677973366e38) // largest double rounding down to FLT_MAX
	add(3.4028235677973370e38) // just above: rounds to +inf
	add(3.5e38)
	add(-3.5e38)
	add(1.1754943508222875e-38) // FLT_MIN normal
	add(1.1754943508222874e-38) // just below: subnormal
	add(1.401298464324817e-45)  // smallest subnormal
	add(7.006492321624085e-46)  // half the smallest: rounds to even (zero)
	add(2.1019484743895325e-45) // 1.5 quanta: round half to even
	add(-1.401298464324817e-45)
	add(math.SmallestNonzeroFloat64)
	// A spread of magnitudes with messy mantissas.
	for e := -40; e <= 38; e += 3 {
		add(1.2345678901234567 * math.Pow(10, float64(e)))
		add(-9.876543210987654 * math.Pow(10, float64(e)))
	}
	// A deterministic random sweep across the whole exponent range. Boundary
	// cases prove the edges; this proves the middle, where a subtly wrong
	// rounding mode would otherwise hide.
	rng := rand.New(rand.NewSource(20260728))
	for i := 0; i < 3000; i++ {
		// Random mantissa, exponent spanning subnormal to overflow.
		exp := rng.Intn(300) - 150
		m := rng.Float64()*2 - 1
		add(m * math.Pow(2, float64(exp)))
	}

	var b strings.Builder
	b.WriteString(luart.Prelude())
	b.WriteString("\nlocal out = {}\n")
	for _, v := range vals {
		// Hex float literals are exact; decimal would introduce a second
		// rounding and make the comparison meaningless.
		b.WriteString(fmt.Sprintf("out[#out+1] = string.format('%%.17g', f32(%s))\n",
			luaFloat(v)))
	}
	b.WriteString("print(table.concat(out, '\\n'))\n")

	stdout, err := h.RunString(b.String())
	if err != nil {
		t.Fatalf("running f32 rounding: %v", err)
	}
	got := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(got) != len(vals) {
		t.Fatalf("expected %d results, got %d", len(vals), len(got))
	}

	bad := 0
	for i, v := range vals {
		want := float64(float32(v))
		g, err := strconv.ParseFloat(strings.TrimSpace(got[i]), 64)
		if err != nil {
			// inf/nan render as words in Lua.
			switch strings.TrimSpace(got[i]) {
			case "inf":
				g = math.Inf(1)
			case "-inf":
				g = math.Inf(-1)
			case "nan", "-nan":
				g = math.NaN()
			default:
				t.Errorf("value %g: unparsable result %q", v, got[i])
				continue
			}
		}
		if math.IsNaN(want) && math.IsNaN(g) {
			continue
		}
		// Compare bit patterns so -0.0 and +0.0 are distinguished.
		if math.Float64bits(g) != math.Float64bits(want) {
			bad++
			if bad <= 10 {
				t.Errorf("f32(%.17g):\n  got  %.17g\n  want %.17g", v, g, want)
			}
		}
	}
	if bad > 10 {
		t.Errorf("... and %d more mismatches", bad-10)
	}
	t.Logf("checked %d values, %d mismatches", len(vals), bad)
}

// luaFloat renders a float64 as an exact Lua literal.
//
// Hex float form is used for finite values because it round-trips exactly;
// Factorio's tostring gives only 14 significant digits, so decimal emission
// silently loses precision.
func luaFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "math.huge"
	case math.IsInf(v, -1):
		return "-math.huge"
	case math.IsNaN(v):
		return "(0/0)"
	}
	s := strconv.FormatFloat(v, 'x', -1, 64)
	if strings.HasPrefix(s, "-") {
		return "(" + s + ")"
	}
	return s
}
