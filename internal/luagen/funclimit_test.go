package luagen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// TestTheJumpLimitIsWhereLuaPutsIt pins luaMaxJumpInstructions against the real
// parser, at the instruction, in both directions.
//
// The constant is read out of lopcodes.h (SIZE_Bx = 9 + 9, MAXARG_sBx =
// MAXARG_Bx >> 1), and a constant read out of a header is a constant nobody
// checked. Both halves are asserted: one instruction under the limit must load,
// and one over must be refused with Lua's own words. A test that only checked
// the refusal would pass against a limit that was too tight by any amount.
func TestTheJumpLimitIsWhereLuaPutsIt(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// One forward goto over n statements. `x = x + 1` with x a local and 1 a
	// constant is exactly one ADD, so n statements are n VM instructions and
	// the jump's offset is n.
	build := func(n int) string {
		var b strings.Builder
		b.WriteString("local function f(a)\nlocal x = 0\nif a then goto DONE end\n")
		for i := 0; i < n; i++ {
			b.WriteString("x = x + 1\n")
		}
		b.WriteString("::DONE::\nreturn x\nend\nprint('loaded')\n")
		return b.String()
	}

	out, err := h.RunString(build(luaMaxJumpInstructions))
	if err != nil || !strings.Contains(out, "loaded") {
		t.Fatalf("a jump over exactly %d instructions was refused, so the limit "+
			"is TIGHTER than lopcodes.h says: %v %s", luaMaxJumpInstructions, err, out)
	}

	out, err = h.RunString(build(luaMaxJumpInstructions + 1))
	if err == nil && strings.Contains(out, "loaded") {
		t.Fatalf("a jump over %d instructions loaded, so the limit is LOOSER "+
			"than lopcodes.h says and luaMaxJumpInstructions is wrong",
			luaMaxJumpInstructions+1)
	}
	if !strings.Contains(err.Error()+out, "control structure too long") {
		t.Fatalf("expected Lua's own \"control structure too long\"; got %v %s", err, out)
	}
}

// TestAnOverlongJumpSpanIsRefusedAtPackageTime is the check doing its job: a
// module whose emitted function carries one jump past the threshold is refused
// by the emitter, with a message naming the function and the remedy.
//
// The shape is the downstream one exactly -- a br_if near the top of a long
// block, and the block's end label sitting on the trap that every failing edge
// was merged into.
func TestAnOverlongJumpSpanIsRefusedAtPackageTime(t *testing.T) {
	_, err := emitLongJump(t, longJumpOps(maxJumpSpanBytes))
	if err == nil {
		t.Fatal("a function whose jump crosses more than the threshold was accepted")
	}
	for _, want := range []string{
		"too big for Lua",
		"control structure too long",
		"//go:noinline",
		"#[inline(never)]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	// It must name the function. The wat below exports it as "big", and the
	// name section is what carries that through to the diagnostic.
	if !strings.Contains(err.Error(), "big") {
		t.Errorf("the refusal names no function:\n%v", err)
	}
}

// TestWhatTheCheckRefusesIsWhatLuaRefuses is the red proof, and it is the only
// test here that has to defeat the check to make its point.
//
// It raises the threshold far enough that the emitter hands back the text of a
// module it would normally refuse, gives that text to bin/lua52f, and requires
// Lua to refuse it in its own words. Without this the check is a number
// compared against another number.
func TestWhatTheCheckRefusesIsWhatLuaRefuses(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	saved := maxJumpSpanBytes
	maxJumpSpanBytes = 1 << 30
	t.Cleanup(func() { maxJumpSpanBytes = saved })

	// Comfortably past Lua's own limit rather than past ours: the point is what
	// LUA does, and the two thresholds are deliberately not the same number.
	src, err := emitLongJump(t, longJumpOps(luaMaxJumpInstructions*8))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if span := maxJumpSpan(src); span.bytes <= saved {
		t.Fatalf("the fixture is not over the shipped threshold (%d bytes vs %d), "+
			"so this proves nothing", span.bytes, saved)
	}

	out, err := h.RunString("local M = (function(...)\n" + src + "\nend)()\nprint('loaded')\n")
	if err == nil && strings.Contains(out, "loaded") {
		t.Fatal("Lua accepted a chunk the check refuses, so the threshold is too tight")
	}
	if !strings.Contains(err.Error()+out, "control structure too long") {
		t.Fatalf("Lua refused it for some OTHER reason, so this says nothing about "+
			"the jump limit: %v %s", err, out)
	}
}

// TestAJumplessFunctionIsNotRefusedHoweverBig is the property that stops this
// check being "simplified" into a check on a function's size.
//
// Lua's limit is on ONE JUMP'S SPAN. A function with no jump in it has no span
// to overflow, and that is not a technicality: the reproduction that found this
// defect, built without its bounds checks, emits a 140,998-INSTRUCTION function
// whose widest jump is ZERO, and it loads. A size check would refuse it for
// nothing.
func TestAJumplessFunctionIsNotRefusedHoweverBig(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	var body strings.Builder
	for i := 0; i < longJumpOps(maxJumpSpanBytes*2); i++ {
		body.WriteString(" (local.set 0 (i32.add (local.get 0) (i32.const 1)))")
	}
	src, err := emitWAT(t, `(module (memory 1) (func $flat (export "flat") (result i32)
		(local i32)`+body.String()+` (local.get 0)))`)
	if err != nil {
		t.Fatalf("a function with no jump in it was refused: %v", err)
	}
	if span := maxJumpSpan(src); span.bytes != 0 {
		t.Fatalf("the fixture has a jump after all (%d bytes to %s), so it does "+
			"not test what it claims", span.bytes, span.label)
	}
	if len(src) <= maxJumpSpanBytes {
		t.Fatalf("the fixture is only %d bytes, under the %d-byte threshold, so a "+
			"size check would have accepted it too and this proves nothing",
			len(src), maxJumpSpanBytes)
	}
	out, err := h.RunString("local M = (function(...)\n" + src + "\nend)()\nprint('loaded')\n")
	if err != nil || !strings.Contains(out, "loaded") {
		t.Fatalf("Lua refused a jumpless function of %d bytes: %v %s", len(src), err, out)
	}
}

// TestABackwardJumpCountsToo covers the other sign. Lua's test is abs(offset),
// so a long backward branch to a loop header is the same defect from the other
// end, and a check that only looked forward would miss every long loop.
func TestABackwardJumpCountsToo(t *testing.T) {
	// A loop whose body is long and whose back edge is at the bottom.
	var body strings.Builder
	for i := 0; i < longJumpOps(maxJumpSpanBytes); i++ {
		body.WriteString(" (local.set 1 (i32.add (local.get 1) (i32.const 1)))")
	}
	_, err := emitWAT(t, `(module (memory 1) (func $back (export "back") (param i32) (result i32)
		(local i32)
		(loop $l`+body.String()+`
		  (br_if $l (local.get 0)))
		(local.get 1)))`)
	if err == nil {
		t.Fatal("a long BACKWARD jump was accepted; the check is looking one way only")
	}
	if !strings.Contains(err.Error(), "too big for Lua") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestEveryGuestThisRepoEmitsIsUnderTheJumpLimit is the corpus half: the check
// must not refuse anything this project actually ships, and the margin it has
// today is worth logging rather than leaving to be rediscovered.
//
// BOTH TOOLCHAINS, because the widest span in the whole corpus is a RUST one --
// guest/rust ./examples/array at -opt=3 -- and a Go-only audit would quote a
// margin 25% wider than the one that exists.
//
// It counts what it audited and fails on zero, because a corpus test that
// matched nothing passes forever -- the habit agents/testing.md records after
// this repo was bitten by it three ways.
func TestEveryGuestThisRepoEmitsIsUnderTheJumpLimit(t *testing.T) {
	root := luagenRepoRoot(t)
	tmp := t.TempDir()

	audited, widest, widestName := 0, 0, ""
	check := func(name, wasmPath string) {
		raw, err := os.ReadFile(wasmPath)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		m, err := wasm.Decode(raw)
		if err != nil {
			t.Errorf("%s: decode: %v", name, err)
			return
		}
		im, err := ir.BuildModule(m)
		if err != nil {
			t.Errorf("%s: ir: %v", name, err)
			return
		}
		// -opt=3 is the widest output the emitter produces, so it is the level
		// the margin should be quoted at.
		src, err := EmitModuleWith(im, Options{Opt: analysis.O3})
		if err != nil {
			t.Errorf("%s at -opt=3: %v", name, err)
			return
		}
		audited++
		for _, fn := range strings.Split(src, "\nend\n") {
			if sp := maxJumpSpan(fn); sp.bytes > widest {
				widest, widestName = sp.bytes, name
			}
		}
	}

	if ok, why := guest.Available(); !ok {
		t.Logf("skipping the TinyGo half: %s", why)
	} else {
		for _, g := range guardCorpus {
			name := g.dir + " " + g.pkg
			out := filepath.Join(tmp, strings.NewReplacer("/", "-", ".", "").Replace(name)+".wasm")
			if err := guest.Build(filepath.Join(root, filepath.FromSlash(g.dir)), g.pkg, out); err != nil {
				t.Errorf("building %s: %v", name, err)
				continue
			}
			check(name, out)
		}
	}
	if ok, why := guest.RustAvailable(); !ok {
		t.Logf("skipping the Rust half: %s", why)
	} else {
		for _, g := range guardCorpusRust {
			if g.lower || g.collected {
				// The lowered bench crate and the collected arm are the same
				// source through a different back end; neither widens the span
				// and both cost a build.
				continue
			}
			name := g.workspace + " " + g.pkg
			p, err := guest.BuildRust(filepath.Join(root, filepath.FromSlash(g.workspace)),
				g.pkg, filepath.Join(tmp, "cargo"))
			if err != nil {
				t.Errorf("building %s: %v", name, err)
				continue
			}
			check(name, p)
		}
	}

	if audited == 0 {
		t.Skip("neither toolchain is installed, so nothing was audited")
	}
	t.Logf("%d guests at -opt=3; widest jump span %d bytes (%s), threshold %d, %.1f%% of it",
		audited, widest, widestName, maxJumpSpanBytes,
		100*float64(widest)/float64(maxJumpSpanBytes))
	if widest == 0 {
		t.Fatal("no guest produced a jump at all, so the scan matched nothing")
	}
}

// longJumpOps is how many one-line wasm ops it takes to span n bytes of emitted
// Lua. Deliberately generous: the fixtures only have to be over a threshold,
// and being under one by a rounding error is a test that quietly stops testing.
func longJumpOps(n int) int { return n/20 + 1000 }

// emitLongJump builds the shape the downstream failure has: a br_if near the
// top of a long block, and the block's end label sitting on the trap.
func emitLongJump(t *testing.T, ops int) (string, error) {
	t.Helper()
	var body strings.Builder
	for i := 0; i < ops; i++ {
		body.WriteString(" (local.set 1 (i32.add (local.get 1) (i32.const 1)))")
	}
	return emitWAT(t, `(module (memory 1) (func $big (export "big") (param i32) (result i32)
		(local i32)
		(block $out
		  (br_if $out (local.get 0))`+body.String()+`
		  (return (local.get 1)))
		(unreachable)))`)
}

func emitWAT(t *testing.T, src string) (string, error) {
	t.Helper()
	m, err := wasm.DecodeWAT(src)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	return EmitModule(im)
}
