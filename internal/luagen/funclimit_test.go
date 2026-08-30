package luagen

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// TestAnOverlongJumpSpanIsRefusedAtPackageTime is the check doing its job --
// and since the relay it is the BACKSTOP doing its job, which is a narrower
// claim and needs a narrower fixture.
//
// An ordinary over-long jump is no longer refused: it is RELAYED, and the two
// tests below prove that end to end against the real parser. What is left for
// the check is the one shape the relay cannot help, which the design named
// before either was built -- a single basic block longer than the limit, where
// the whole span sits inside one nested construct and there is nowhere at body
// level to hang a trampoline. This body is exactly that: the goto is inside the
// `if`, the filler is inside the `if`, and the only body-level line between the
// goto and its label is the `end` that closes the block, which is not a
// statement boundary at all.
func TestAnOverlongJumpSpanIsRefusedAtPackageTime(t *testing.T) {
	saved := maxJumpSpanBytes
	maxJumpSpanBytes = 40000
	t.Cleanup(func() { maxJumpSpanBytes = saved })

	var b strings.Builder
	b.WriteString("F[0] = function(v0)\n  local v1 = 0\n  if v0 == 0 then\n")
	b.WriteString("    if v1 ~= 0 then goto L0 end\n")
	for i := 0; i < 4000; i++ {
		b.WriteString("    v1 = (v1 + 1) % 4294967296.0\n")
	}
	b.WriteString("  end\n  ::L0::\n  return v1\nend\n")
	body := b.String()

	if sp := maxJumpSpan(body); sp.bytes <= maxJumpSpanBytes {
		t.Fatalf("the fixture's span is only %d bytes against a threshold of %d, so "+
			"there is nothing here to refuse", sp.bytes, maxJumpSpanBytes)
	}
	if out, ok := relayJumps(body); ok || out != body {
		t.Fatalf("the relay claims it can break up a span with no body-level "+
			"statement inside it, so this fixture no longer tests the backstop "+
			"(changed=%v, %d bytes -> %d)", ok, len(body), len(out))
	}

	_, err := relayOrRefuse("big", body)
	if err == nil {
		t.Fatal("a function whose jump crosses more than the threshold, and which " +
			"the relay cannot help, was accepted")
	}
	var js *JumpSpanError
	if !errors.As(err, &js) {
		t.Fatalf("the refusal is not a *JumpSpanError, so a caller cannot tell it "+
			"from any other failure: %T %v", err, err)
	}
	if js.Func != "big" || js.Label != "L0" {
		t.Errorf("the typed error carries func=%q label=%q, want big and L0", js.Func, js.Label)
	}
	for _, want := range []string{
		"too big for Lua",
		"control structure too long",
		"tried to RELAY",
		"//go:noinline",
		"#[inline(never)]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	// It must name the function, or the diagnostic points at nothing.
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

// TestABackwardJumpCountsToo covers the other sign, in both halves. Lua's test
// is abs(offset), so a long backward branch to a loop header is the same defect
// from the other end -- a scan that only looked forward would miss every long
// loop, and a relay that only relayed forward would leave one refused.
//
// The measurement is taken with the threshold raised, which is the only way to
// obtain the text the relay is about to rewrite: raised, the check passes, the
// relay never runs, and the emitted function is the one the scan has to measure.
// Then the same module is packaged at the shipped threshold and has to come out
// carrying trampolines and loading under the real parser.
func TestABackwardJumpCountsToo(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// A loop whose body is long and whose back edge is at the bottom.
	var body strings.Builder
	for i := 0; i < longJumpOps(maxJumpSpanBytes); i++ {
		body.WriteString(" (local.set 1 (i32.add (local.get 1) (i32.const 1)))")
	}
	wat := `(module (memory 1) (func $back (export "back") (param i32) (result i32)
		(local i32)
		(loop $l` + body.String() + `
		  (br_if $l (local.get 0)))
		(local.get 1)))`

	saved := maxJumpSpanBytes
	maxJumpSpanBytes = 1 << 30
	raw, err := emitWAT(t, wat)
	maxJumpSpanBytes = saved
	if err != nil {
		t.Fatalf("emit with the threshold raised: %v", err)
	}
	worst, backward := funcSpan{}, false
	for _, fn := range strings.Split(raw, "\nend\n") {
		if sp := maxJumpSpan(fn); sp.bytes > worst.bytes {
			worst = sp
			// The label is BEHIND the goto, which is what makes it backward.
			lines := scanLuaLines(fn)
			labels, chain := labelIndex(lines)
			for _, l := range lines {
				if n, ok := gotoOn(l.text); ok && resolveChain(n, chain) == sp.label {
					backward = labels[sp.label] < l.off
				}
			}
		}
	}
	if worst.bytes <= maxJumpSpanBytes {
		t.Fatalf("the fixture's widest span is only %d bytes against a threshold of "+
			"%d, so nothing here is long enough to be measured", worst.bytes, maxJumpSpanBytes)
	}
	if !backward {
		t.Fatalf("the widest span in the fixture (%d bytes to %s) is a FORWARD jump, "+
			"so this says nothing about the other sign", worst.bytes, worst.label)
	}

	src, err := emitWAT(t, wat)
	if err != nil {
		t.Fatalf("a long backward jump was refused rather than relayed: %v", err)
	}
	if !strings.Contains(src, "::"+relayTrampolineName(0)+"::") {
		t.Fatal("the packaged module carries no trampoline, so the backward span " +
			"came out under the threshold by itself and nothing was relayed")
	}
	assertRelayed(t, h, src)
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

// TestARelayedJumpLoadsWhereTheDirectOneDoesNot is the relay's whole claim,
// against the real parser and in both directions of it.
//
// One module, emitted twice. With the threshold raised the check passes, the
// relay never runs, and Lua refuses the result in its own words -- which is
// TestWhatTheCheckRefusesIsWhatLuaRefuses' fixture and is what says the span is
// genuinely past Lua's limit rather than only past ours. At the shipped
// threshold the same module is relayed and LOADS. Nothing about the wasm
// changed between the two; the ladder is the only difference.
func TestARelayedJumpLoadsWhereTheDirectOneDoesNot(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// Comfortably past LUA's limit rather than past ours: the point is what the
	// parser does, and the two thresholds are deliberately different numbers.
	ops := longJumpOps(luaMaxJumpInstructions * 8)

	saved := maxJumpSpanBytes
	maxJumpSpanBytes = 1 << 30
	direct, err := emitLongJump(t, ops)
	maxJumpSpanBytes = saved
	if err != nil {
		t.Fatalf("emit with the threshold raised: %v", err)
	}
	out, err := h.RunString("local M = (function(...)\n" + direct + "\nend)()\nprint('loaded')\n")
	if err == nil && strings.Contains(out, "loaded") {
		t.Fatal("Lua accepted the UNRELAYED module, so the fixture is not over Lua's " +
			"own limit and the relay is not what makes the difference below")
	}
	if !strings.Contains(err.Error()+out, "control structure too long") {
		t.Fatalf("Lua refused the unrelayed module for some OTHER reason, so this "+
			"says nothing about the jump limit: %v %s", err, out)
	}

	relayed, err := emitLongJump(t, ops)
	if err != nil {
		t.Fatalf("a module the relay should have rescued was refused: %v", err)
	}
	if !strings.Contains(relayed, "::"+relayTrampolineName(0)+"::") {
		t.Fatal("the packaged module carries no trampoline, so whatever made it " +
			"acceptable was not the relay")
	}
	if sp := maxJumpSpan(relayed); sp.bytes > maxJumpSpanBytes {
		t.Fatalf("the relayed module still carries a %d-byte span to %s", sp.bytes, sp.label)
	}
	assertRelayed(t, h, relayed)
	t.Logf("%d bytes direct (refused by Lua) -> %d bytes relayed (loads), "+
		"widest span %d against a threshold of %d",
		len(direct), len(relayed), maxJumpSpan(relayed).bytes, maxJumpSpanBytes)
}

// TestARelayedFunctionComputesTheSameAnswer is the correctness half, and it is
// the reason the measurement harness printed a return value rather than only
// "LOADS".
//
// A relay that broke the straight-line path would still load: the trampolines
// are syntactically fine and the chunk parses either way. What it would do is
// fall into a trampoline and take the jump the ladder was built to relay, which
// on this fixture is the trap. So the fixture is run BEFORE and AFTER, with the
// threshold lowered so that both arms are small enough for Lua to accept, and
// the two answers are compared.
func TestARelayedFunctionComputesTheSameAnswer(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	const ops = 3000
	saved := maxJumpSpanBytes

	maxJumpSpanBytes = 1 << 30
	direct, err := emitLongJump(t, ops)
	maxJumpSpanBytes = saved
	if err != nil {
		t.Fatalf("emit with the threshold raised: %v", err)
	}

	// Low enough that the fixture is well over it and the ladder needs several
	// hops, and high enough that a hop is still many statements.
	maxJumpSpanBytes = 20000
	relayed, err := emitLongJump(t, ops)
	maxJumpSpanBytes = saved
	if err != nil {
		t.Fatalf("emit at a lowered threshold: %v", err)
	}
	if direct == relayed {
		t.Fatal("the two arms are byte-identical, so the relay did not run and " +
			"there is nothing here to compare")
	}
	if n := strings.Count(relayed, "  ::"+relaySeparator); n != 0 {
		t.Fatalf("separator/label ordering is wrong in %d places", n)
	}
	hops := strings.Count(relayed, "\n  "+relaySeparator+"\n")
	if hops < 4 {
		t.Fatalf("the ladder is only %d hops long, which is not enough to say the "+
			"chain works rather than a single relay", hops)
	}

	answer := func(what, src string) string {
		out, err := h.RunString("local M = (function(...)\n" + src +
			"\nend)()\nprint(M.exports[\"big\"](0))\n")
		if err != nil {
			t.Fatalf("%s did not run: %v %s", what, err, out)
		}
		return strings.TrimSpace(out)
	}
	got, want := answer("the relayed module", relayed), answer("the direct module", direct)
	if got != want {
		t.Fatalf("the relayed function returns %s where the direct one returns %s -- "+
			"control fell into a trampoline instead of stepping over it", got, want)
	}
	t.Logf("%d hops, both arms return %s", hops, want)
}

// TestATrampolineIsNeverFallenInto is the guard, as a TEXT property.
//
// A trampoline is inserted at a REACHABLE point, so the straight-line path walks
// straight into it unless something jumps over it. That something is one
// unconditional goto per station, and this is a text property or it is nothing:
// a missing guard costs an answer on one fixture and is invisible on every
// fixture whose straight-line path happens not to reach that far.
func TestATrampolineIsNeverFallenInto(t *testing.T) {
	saved := maxJumpSpanBytes
	maxJumpSpanBytes = 20000
	src, err := emitLongJump(t, 3000)
	maxJumpSpanBytes = saved
	if err != nil {
		t.Fatalf("emit at a lowered threshold: %v", err)
	}

	lines := scanLuaLines(src)
	seen := 0
	for i, l := range lines {
		name, ok := labelDefinedOn(l.text)
		if !ok || !strings.HasPrefix(name, "LT") || strings.HasPrefix(name, "LTs") {
			continue
		}
		seen++
		k := strings.TrimPrefix(name, "LT")
		skip := relaySkipName(0)
		skip = skip[:len(skip)-1] + k

		p := previousCodeLine(lines, i)
		if p < 0 || lines[p].text != "goto "+skip {
			got := "<nothing>"
			if p >= 0 {
				got = lines[p].text
			}
			t.Errorf("::%s:: is preceded by %q and not by its guard `goto %s`, so "+
				"control falls straight into it", name, got, skip)
			continue
		}
		// ...and the station ends by putting control back on the straight line.
		want := []string{relaySeparator, "goto ", "::" + skip + "::"}
		for d, w := range want {
			if i+1+d >= len(lines) || !strings.HasPrefix(lines[i+1+d].text, w) {
				t.Errorf("the station at ::%s:: is not `%s` then a goto then "+
					"::%s::", name, relaySeparator, skip)
				break
			}
		}
	}
	if seen == 0 {
		t.Fatal("no trampoline was emitted at all, so this asserted nothing")
	}
	t.Logf("%d trampolines, every one guarded", seen)
}

// TestALabelImmediatelyFollowedByAGotoIsMeasuredAsAChain is the incompleteness
// fix, and it is the one test here that is about Lua rather than about us.
//
// A ladder whose every hop is far under the limit is refused anyway when the
// hops CHAIN -- `::T:: goto U` with nothing between them merges the two pending
// jump lists and the whole ladder is patched as one jump. The pre-2026-08-30
// scan measured one goto-to-label distance and passed exactly that program.
//
// Both halves are asserted, and the second is what makes the first evidence:
// the scan must report the chained TOTAL for a bare ladder and one HOP for a
// separated one, and bin/lua52f must agree by refusing the first and running the
// second.
func TestALabelImmediatelyFollowedByAGotoIsMeasuredAsAChain(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}

	// Three hops of 50,000 instructions: every hop is comfortably under Lua's
	// 131,071 and the total is comfortably over it.
	const hops, fill = 3, 50000
	bare, separated := ladderChunk(hops, fill, ""), ladderChunk(hops, fill, relaySeparator)

	bareSpan, sepSpan := maxJumpSpan(bare), maxJumpSpan(separated)
	if bareSpan.bytes < 2*sepSpan.bytes {
		t.Fatalf("the scan measures the bare ladder at %d bytes and the separated "+
			"one at %d -- it is measuring one hop in both, so a chain of short "+
			"jumps still reads as short and Lua will refuse what this passes",
			bareSpan.bytes, sepSpan.bytes)
	}
	if bareSpan.label != "L0" {
		t.Errorf("the bare ladder's widest span reports label %q; the chain ends at "+
			"L0 and that is what a jump into it really reaches", bareSpan.label)
	}

	out, err := h.RunString(bare)
	if err == nil && !strings.Contains(out, "control structure too long") {
		t.Fatalf("Lua ACCEPTED a bare %d-hop ladder of %d instructions each, so the "+
			"chaining this models does not happen and the scan is now too strict: %s",
			hops, fill, out)
	}
	if !strings.Contains(err.Error()+out, "control structure too long") {
		t.Fatalf("Lua refused the bare ladder for some other reason: %v %s", err, out)
	}

	out, err = h.RunString(separated)
	if err != nil {
		t.Fatalf("Lua refused a SEPARATED ladder of the same total length, so the "+
			"one statement between the label and the goto does not discharge the "+
			"pending list and the whole relay rests on nothing: %v %s", err, out)
	}
	if got := strings.TrimSpace(out); got != strconv.Itoa(hops*fill) {
		t.Fatalf("the separated ladder ran but returned %q, want %d -- it took the "+
			"relay instead of the straight-line path", got, hops*fill)
	}
	t.Logf("bare ladder measured at %d bytes and refused by Lua; separated at %d "+
		"bytes and returns %d", bareSpan.bytes, sepSpan.bytes, hops*fill)
}

// TestATrampolineLabelCannotCollideWithAnother is the namespace audit for the
// relay's two families, and it is the guard's `g%d` lesson applied before the
// fact rather than after it.
//
// A trampoline index is a dense small number counted per function, so it is the
// same hazard class as a step index: `LT3` against a branch label `L3` is one
// character away from a silent miscompile, and `LTs3` against the loop guard's
// `ls3_0` is one case change away. Both families are enumerated in nameFamilies
// so the general audit covers them, and this asserts they are actually IN it --
// without which the general test would pass by asserting less.
func TestATrampolineLabelCannotCollideWithAnother(t *testing.T) {
	fams := nameFamilies()
	relays := 0
	for _, f := range fams {
		if strings.HasPrefix(f.what, "a relay") {
			relays++
		}
	}
	if relays != 2 {
		t.Fatalf("nameFamilies enumerates %d relay families, want 2 -- the "+
			"trampoline label and the skip label", relays)
	}

	mine := map[string]bool{}
	for i := 0; i < 256; i++ {
		mine[relayTrampolineName(i)] = true
		mine[relaySkipName(i)] = true
	}
	if len(mine) != 512 {
		t.Fatalf("the two relay families produce %d distinct names over 256 indices, "+
			"want 512 -- they collide with each other", len(mine))
	}
	for _, f := range fams {
		if strings.HasPrefix(f.what, "a relay") {
			continue
		}
		for _, n := range f.names() {
			if mine[n] {
				t.Errorf("%q is both a relay label and %s -- whichever is declared "+
					"in the narrower scope shadows the other silently", n, f.what)
			}
		}
	}
}

// TestEveryGuestThisRepoEmitsIsUnchangedByTheRelay is the assertion that says
// the feature is FREE.
//
// Nothing this repo emits is anywhere near the threshold -- the widest span is
// 38% of it -- so the relay must be a byte-for-byte no-op on all of it. Called
// directly rather than through EmitModuleWith, because through the emitter the
// relay is never reached at all and the test would be asserting the early-out in
// checkJumpSpan rather than the transform.
//
// BOTH TOOLCHAINS, for the reason the jump-limit corpus test gives: the widest
// span in the whole corpus is a Rust one.
func TestEveryGuestThisRepoEmitsIsUnchangedByTheRelay(t *testing.T) {
	root := luagenRepoRoot(t)
	tmp := t.TempDir()

	audited, funcs := 0, 0
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
		// -opt=3 is the widest output the emitter produces.
		src, err := EmitModuleWith(im, Options{Opt: analysis.O3})
		if err != nil {
			t.Errorf("%s at -opt=3: %v", name, err)
			return
		}
		audited++
		if out, changed := relayJumps(src); changed || out != src {
			t.Errorf("%s at -opt=3: the relay rewrote a chunk that is under the "+
				"threshold (changed=%v, %d bytes -> %d)", name, changed, len(src), len(out))
		}
		for _, fn := range strings.Split(src, "\nend\n") {
			funcs++
			if out, changed := relayJumps(fn); changed || out != fn {
				t.Errorf("%s at -opt=3: the relay rewrote a function that is under "+
					"the threshold (changed=%v)", name, changed)
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
	t.Logf("%d guests at -opt=3, %d function texts; the relay changed none of them",
		audited, funcs)
}

// TestARelayedRealGuestStillParses is the other half of the corpus claim, and it
// is the only test here that runs the relay over code the emitter really writes.
//
// TestEveryGuestThisRepoEmitsIsUnchangedByTheRelay proves the transform is a
// no-op at the shipped threshold, which is a statement about the early-out. It
// says nothing about what the relay does when it FIRES, and every fixture that
// does fire it is a four-line hand-written WAT: one block, one br_if, no nested
// `if`, no `do return end`, no branch table, no loop guard, no spilled frame. But
// insertionPoints' three rules -- depth exactly 1, never before a block closer,
// never inside the prologue -- exist for exactly those shapes, and being wrong
// about one of them emits Lua that does not parse. That failure is loud, and it
// is loud at PACKAGE TIME for the one author the feature exists for.
//
// So the threshold is driven DOWN until the relay fires on every guest in the
// corpus, and each result is handed to the real parser.
//
// IT ASKS WHETHER THE CHUNK PARSES, NOT WHETHER IT RUNS. A guest chunk calls
// fk_import for host functions this harness does not supply, so running one is a
// runtime failure that says nothing about the relay -- and the first draft of
// this test reported all twelve guests as broken for that reason. Reaching
// fk_import is proof the whole chunk compiled, which is the question.
func TestARelayedRealGuestStillParses(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	root := luagenRepoRoot(t)
	tmp := t.TempDir()

	saved := maxJumpSpanBytes
	defer func() { maxJumpSpanBytes = saved }()

	audited, relayed, stations := 0, 0, 0
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
		// Two thresholds rather than one: the lower makes the relay fire in far
		// more functions, and the higher is nearer the shape a real over-limit
		// guest has. Below about 2 KB most guests are REFUSED instead -- a hop
		// window that small often holds no insertion point at all -- and a
		// refusal is the check doing its job, not a failure of this test.
		for _, thr := range []int{2000, 20000} {
			maxJumpSpanBytes = thr
			src, err := EmitModuleWith(im, Options{Opt: analysis.O3})
			if err != nil {
				continue // refused; the backstop, and this test is not about it
			}
			audited++
			n := strings.Count(src, "\n  "+relaySeparator+"\n")
			if n == 0 {
				continue
			}
			relayed++
			stations += n
			out, rerr := h.RunString("local M = (function(...)\n" + src + "\nend)()\n")
			all := out
			if rerr != nil {
				all += rerr.Error()
			}
			if strings.Contains(all, "control structure too long") {
				t.Errorf("%s at threshold %d: %d stations and Lua STILL refuses the "+
					"jump -- the ladder did not break it", name, thr, n)
				continue
			}
			// "was not supplied" is fk_import, which is thousands of statements
			// into the chunk: the whole thing compiled.
			if rerr != nil && !strings.Contains(all, "was not supplied") {
				t.Errorf("%s at threshold %d: %d stations and the chunk DOES NOT "+
					"PARSE: %v %s", name, thr, n, rerr, out)
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
	// The vacuity guard, and it is the whole reason this test is not a duplicate
	// of the no-op one: if the relay never fired, every chunk above parsed
	// because the emitter had not touched it.
	if relayed == 0 {
		t.Fatal("the relay did not fire on a single guest at any threshold, so this " +
			"audited nothing that the no-op corpus test does not already cover")
	}
	t.Logf("%d emissions, %d of them relayed, %d stations in total, every one parses",
		audited, relayed, stations)
}

// assertRelayed loads a relayed chunk under the real parser and requires it to
// carry the shape the relay promises.
func assertRelayed(t *testing.T, h *luahost.Host, src string) {
	t.Helper()
	out, err := h.RunString("local M = (function(...)\n" + src + "\nend)()\nprint('loaded')\n")
	if err != nil || !strings.Contains(out, "loaded") {
		t.Fatalf("Lua refused the RELAYED module: %v %s", err, out)
	}
	if n := strings.Count(src, "\n  "+relaySeparator+"\n"); n == 0 {
		t.Fatal("the relayed module carries no separating statement, so its hops " +
			"would chain into one jump again")
	}
}

// ladderChunk is a hand-written relay ladder: `hops` trampolines, each guarded
// the way the emitter guards one, with `fill` single-instruction statements
// between them and `sep` between each label and its goto.
//
// Hand-written rather than emitted because the point is what LUA does with the
// shape, and the shape has to be obtainable with the separator REMOVED -- which
// the emitter will not produce.
func ladderChunk(hops, fill int, sep string) string {
	var b strings.Builder
	b.WriteString("local v = 0\nif v ~= 0 then goto LT0 end\n")
	for i := 0; i < hops; i++ {
		for j := 0; j < fill; j++ {
			b.WriteString("v = v + 1\n")
		}
		to := "L0"
		if i+1 < hops {
			to = relayTrampolineName(i + 1)
		}
		b.WriteString("goto " + relaySkipName(i) + "\n")
		b.WriteString("::" + relayTrampolineName(i) + "::\n")
		if sep != "" {
			b.WriteString(sep + "\n")
		}
		b.WriteString("goto " + to + "\n")
		b.WriteString("::" + relaySkipName(i) + "::\n")
	}
	b.WriteString("print(v)\ndo return end\n::L0::\nprint(-1)\n")
	return b.String()
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
