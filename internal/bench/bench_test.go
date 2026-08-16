package bench

import (
	"strings"
	"testing"
)

func TestParseOutput(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		checksum string
		ops      int64
		wantErr  string
	}{
		{name: "tab separated", in: "89156008\t2000", checksum: "89156008", ops: 2000},
		{name: "trailing newline", in: "42\t10\n", checksum: "42", ops: 10},
		{name: "float checksum", in: "3.141593\t1000", checksum: "3.141593", ops: 1000},
		{name: "missing ops", in: "42", wantErr: "expected"},
		{name: "extra field", in: "42\t10\t7", wantErr: "expected"},
		{name: "non-numeric ops", in: "42\tmany", wantErr: "bad op count"},
		{name: "zero ops", in: "42\t0", wantErr: "must be positive"},
		{name: "negative ops", in: "42\t-3", wantErr: "must be positive"},
		{name: "empty", in: "", wantErr: "expected"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sum, ops, err := parseOutput(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sum != tc.checksum || ops != tc.ops {
				t.Errorf("got (%q, %d), want (%q, %d)", sum, ops, tc.checksum, tc.ops)
			}
		})
	}
}

// A variant that computes a different answer is not a faster variant. This is
// the guard that stops a broken lowering from looking like an optimization.
func TestCheckChecksums(t *testing.T) {
	agree := []Result{
		{Kernel: "sum", Variant: "nat", Checksum: "100"},
		{Kernel: "sum", Variant: "gen", Checksum: "100"},
		{Kernel: "fib", Variant: "nat", Checksum: "832040"},
	}
	if bad := CheckChecksums(agree); len(bad) != 0 {
		t.Errorf("expected no mismatches, got %v", bad)
	}

	disagree := []Result{
		{Kernel: "sum", Variant: "nat", Checksum: "100"},
		{Kernel: "sum", Variant: "gen", Checksum: "101"},
	}
	bad := CheckChecksums(disagree)
	if len(bad) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(bad), bad)
	}
	if !strings.Contains(bad[0], "gen") || !strings.Contains(bad[0], "sum") {
		t.Errorf("message should name the kernel and variant: %q", bad[0])
	}
}

func TestGate(t *testing.T) {
	kernels := []Kernel{
		{Name: "sum", MaxRatio: 15},
		{Name: "dot"}, // informational: no limit
	}

	within := []Result{
		{Kernel: "sum", Variant: "nat", Ratio: 1},
		{Kernel: "sum", Variant: "gen", Ratio: 2.91},
		{Kernel: "dot", Variant: "gen", Ratio: 11.18},
	}
	if fails := Gate(within, kernels); len(fails) != 0 {
		t.Errorf("expected no failures, got %v", fails)
	}

	over := []Result{
		{Kernel: "sum", Variant: "gen", Ratio: 15.1},
	}
	fails := Gate(over, kernels)
	if len(fails) != 1 {
		t.Fatalf("expected 1 failure, got %d: %v", len(fails), fails)
	}
	if !strings.Contains(fails[0], "15.1x") {
		t.Errorf("message should report the measured ratio: %q", fails[0])
	}
}

// The baseline variant is what everything else is measured against, so it can
// never itself fail the gate -- its ratio is 1.0 by construction.
func TestGateIgnoresBaseline(t *testing.T) {
	kernels := []Kernel{{Name: "sum", MaxRatio: 1}}
	results := []Result{{Kernel: "sum", Variant: "nat", Ratio: 1.0}}
	if fails := Gate(results, kernels); len(fails) != 0 {
		t.Errorf("baseline should never fail its own gate, got %v", fails)
	}
}

// Every kernel must name a baseline that appears nowhere in its own Variants
// list, or Run would measure it twice and the ratio would be self-referential.
func TestM0KernelsAreWellFormed(t *testing.T) {
	for _, k := range M0Kernels {
		if k.Name == "" || k.File == "" {
			t.Errorf("kernel %+v is missing a name or file", k)
		}
		if k.Baseline == "" {
			t.Errorf("%s: no baseline variant", k.Name)
		}
		if k.Why == "" {
			t.Errorf("%s: no explanation of what it measures", k.Name)
		}
		for _, v := range k.Variants {
			if v == k.Baseline {
				t.Errorf("%s: baseline %q also listed in Variants", k.Name, v)
			}
		}
		if len(k.Variants) == 0 {
			t.Errorf("%s: nothing to compare against the baseline", k.Name)
		}
	}
}

// The two gate kernels must keep a hard limit. If someone relaxes these to make
// a red build green, this test says so.
func TestPrimaryGatesHaveLimits(t *testing.T) {
	want := map[string]bool{"sum": true, "chase": true}
	for _, k := range M0Kernels {
		if want[k.Name] {
			if k.MaxRatio <= 0 {
				t.Errorf("%s is a primary gate and must have a MaxRatio", k.Name)
			}
			if k.MaxRatio > 15 {
				t.Errorf("%s: MaxRatio %.0f exceeds the documented 15x go/no-go",
					k.Name, k.MaxRatio)
			}
			delete(want, k.Name)
		}
	}
	for name := range want {
		t.Errorf("primary gate kernel %q is missing from M0Kernels", name)
	}
}
