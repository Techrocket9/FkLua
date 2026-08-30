package luagen

import (
	"fmt"
	"strconv"
	"strings"
)

// Lua's jump limit, and the check that turns hitting it into a package-time
// error instead of a refusal at the user's game start.
//
// THE MECHANIC, verified against third_party/lua-5.2.1/build/src/lcode.c.
// Every jump in a Lua function is one VM instruction whose signed offset lives
// in the sBx field, and `fixjump` refuses to patch one that does not fit:
//
//	static void fixjump (FuncState *fs, int pc, int dest) {
//	  int offset = dest-(pc+1);
//	  if (abs(offset) > MAXARG_sBx)
//	    luaX_syntaxerror(fs->ls, "control structure too long");
//
// sBx is 18 bits biased (lopcodes.h: SIZE_Bx = SIZE_B + SIZE_C = 9 + 9), so
// MAXARG_sBx is 2^17 - 1 = 131071. THE UNIT IS VM INSTRUCTIONS, NOT LINES AND
// NOT BYTES, and the limit is on ONE JUMP'S SPAN rather than on a function's
// size -- which is why the check below measures spans and not functions.
//
// Measured on bin/lua52f, one forward goto over N single-instruction
// statements: N = 131071 loads and N = 131072 is refused with
// "control structure too long". Both halves matter -- the boundary is exactly
// MAXARG_sBx and there is no slack.
//
// WHAT IT LOOKS LIKE WHEN IT FIRES, measured in a real Factorio 2.1.16 rather
// than quoted. A mod whose data module carries one over-long jump:
//
//	Failed to load mod "fk-jumplimit": __fk-jumplimit__/fk_data.lua:84:
//	__fk-jumplimit__/fk_data_module.lua:163449: control structure too long near '('
//
// It names the GENERATED file and line, and the token the parser was holding
// when the pending gotos were patched -- which is whatever follows the label,
// so for a guest it is often `near 'trap_unreachable'`. What it names nowhere
// is anything in the AUTHOR'S source: there is no route from
// fk_data_module.lua:163449 to the Go or Rust function that produced it, and
// the mod simply does not load. That is the whole reason this check exists, and
// it is the same argument checkChunkLocals is built on one limit over.
//
// WHICH EMITTED CONSTRUCT CARRIES A LONG JUMP. Emission is flat, so a function
// body is a run of statements with `goto L<n>` and `::L<n>::` at body level.
// The span that reaches the limit in practice is a forward goto to a label near
// the END of the function, and it arrives for a reason that has nothing to do
// with the guest author's control flow: LLVM merges every trapping edge in a
// function into ONE block, which the emitter renders as a `::L<n>::` followed
// by `trap_unreachable()` at the bottom. So in a function big enough, the FIRST
// bounds check jumps over the whole body. Measured on a real TinyGo data guest
// of 320 straight-line calls:
//
//	if v2 > 9 then goto L1 end     <- one bounds check, near the top
//	... 26,800 lines ...
//	::L1::
//	trap_unreachable()
//
// A function with no jump at all is unbounded, and that is measured rather than
// assumed: the same guest built without the bounds checks emits a 140,998-
// INSTRUCTION function whose maximum jump span is ZERO, and it loads. Anything
// that checked a function's SIZE would refuse that module for nothing.

// luaMaxJumpInstructions is MAXARG_sBx: the largest jump offset, in VM
// instructions, that Lua 5.2 can encode.
const luaMaxJumpInstructions = 131071

// luaMinBytesPerInstruction is the floor this check converts through, measured
// rather than assumed.
//
// The emitter cannot count VM instructions without modelling Lua's own code
// generator, so the span is measured in EMITTED BYTES and converted with a
// floor on how few bytes of generated Lua one VM instruction can be written in.
//
// HOW IT WAS MEASURED. Every guest this repo can emit was compiled at -opt=0, 2
// and 3 in BOTH languages -- eleven Go examples, eleven Rust ones, both bench
// guests, plus the data-guest reproduction -- and each chunk was dumped under
// bin/lua52f and walked as Lua 5.2 undump output (lundump.c), which gives every
// function's instruction count and the true sBx offset of every jump in it.
// 2,713 emitted functions, 1,931 of them carrying a real jump:
//
//	bytes per instruction of SPAN, over spans of >= 10,000 instructions: 5.606 .. 8.046
//	bytes per instruction of SPAN, over spans of >=  1,000 instructions: min 5.606
//	widest span in the repo's OWN guests: 248,861 bytes / 32,152 instructions
//
// Spans below about 200 bytes run as low as 1.17 bytes per instruction -- a
// backward branch out of a tight loop -- and they are irrelevant here: a span
// of a hundred bytes cannot approach a limit of a hundred thousand
// instructions. The floor that matters is the one over spans big enough to fail.
//
// FIVE is what ships. At 5 bytes per instruction the threshold is 655,355
// bytes, which at the measured 5.606 floor is 116,900 instructions -- 10.8%
// inside Lua's limit. The repo's own widest span is 38% of that threshold, so
// nothing here is anywhere near being refused.
//
// The margin is deliberately in the direction that costs less. Too tight
// refuses a module that would have loaded, with a message naming the remedy;
// too loose lets the cryptic in-game refusal through, which is the failure this
// exists to remove. What the shipped threshold does refuse is the top ~11% of
// what Lua can represent, and a guest in that band is one prototype away from
// not loading at all.
const luaMinBytesPerInstruction = 5

// maxJumpSpanBytes is the byte span at which a function is refused.
//
// A var rather than a const for ONE reason, and it is a test's: proving that
// what this check refuses is what Lua refuses needs the emitted text of a
// module over the limit, and the check is what stops that text existing.
// TestWhatTheCheckRefusesIsWhatLuaRefuses raises it, emits, restores it, and
// hands the result to bin/lua52f. Nothing in any shipped path writes it.
var maxJumpSpanBytes = luaMaxJumpInstructions * luaMinBytesPerInstruction

// funcSpan is the widest goto-to-label distance in one emitted function.
type funcSpan struct {
	bytes int    // the distance, in bytes of emitted Lua
	label string // the label the jump crosses to, for the diagnostic
}

// maxJumpSpan reports the widest distance between a `goto L<n>` and its
// `::L<n>::` in one function's emitted text.
//
// It reads the TEXT rather than being told by each lowering, and that is the
// point: a new lowering that emits a goto is covered without knowing this file
// exists. Generated Lua carries no string literal that could be mistaken for
// either token -- a condition is arithmetic over slots, and the only emitted
// strings are export names and unsupported() messages, neither of which shares
// a line with a jump.
//
// Both directions count. Lua's own test is abs(offset), so a backward goto to a
// loop header at the top of a long body is the same defect seen from the other
// end.
//
// WHAT IT DOES NOT PAIR, and how much that is worth. Three other constructs
// carry an sBx jump the emitter never writes as a goto: a counted loop's
// FORPREP and FORLOOP, which span the `for` body; the implicit jump over a
// multi-line `if ... then ... end`, which the emitter writes for a br_if that
// copies a value and for a loop guard's seed; and a branch table's chain. All
// three are bounded by construction -- a guard seed and a value copy are a
// handful of statements, and a counted loop is a wasm loop, which is wrapped in
// a block whose exit branch this scan DOES pair. That is measured, not argued:
// across the 2,713 emitted functions of the calibration corpus, the largest sBx
// span in any function whose goto-to-label distance was under 200 bytes is
// 42 INSTRUCTIONS, three orders of magnitude below the limit.
// AND THE MODEL WAS INCOMPLETE UNTIL 2026-08-30: A CHAIN OF SHORT JUMPS IS ONE
// LONG JUMP. `::A::` immediately followed by `goto B` -- nothing between the
// label and the jump -- does not produce two independent jumps. `luaK_patchtohere`
// puts the jumps pending at A into `fs->jpc`, and the very next thing emitted is
// `luaK_jump`, which begins
//
//	int jpc = fs->jpc;  /* save list of jumps to here */
//	fs->jpc = NO_JUMP;
//	j = luaK_codeAsBx(fs, OP_JMP, 0, NO_JUMP);
//	luaK_concat(fs, &j, jpc);  /* keep them on hold */
//
// so the two lists MERGE and every jump in them is later patched to B's target.
// A ladder A -> B -> C -> L is therefore one jump from every entry point
// straight to L, and Lua measures it that way.
//
// Measured under bin/lua52f (scratchpad/r4/RESULTS-jumplimit.txt), with every
// individual hop 50,000 instructions and the limit 131,071:
//
//	BARE ladder,  2 hops x 50000 = 100000 total   LOADS, returns 100000
//	BARE ladder,  4 hops x 50000 = 200000 total   REFUSED -- control structure too long
//	BARE ladder, 10 hops x 50000 = 500000 total   REFUSED -- control structure too long
//	SEPARATED ladder, 10 hops x 50000 = 500000    LOADS, returns 500000
//
// The pre-2026-08-30 scan measured one goto-to-label distance and would have
// passed all three of the refused programs. THE BLIND SPOT WAS LATENT AND NOT
// LIVE -- three real guests at -opt=0, 2 and 3 emit ZERO instances of a label
// immediately followed by a bare goto, re-verified on trunk -- and it stopped
// being optional the moment relayJumps below started emitting that shape on
// purpose. The relay's ONE SEPARATING STATEMENT is what discharges `fs->jpc`
// and makes its hops independent; without the fix here the relay would have
// created this blind spot in order to fall into it.
//
// Only a BARE goto chains. `if <cond> then goto X end` emits the test first, and
// any instruction at all discharges the pending list through `dischargejpc`.
// Blank lines, comments and further labels emit nothing, so the scan looks past
// them.
func maxJumpSpan(src string) funcSpan {
	lines := scanLuaLines(src)
	labels, chain := labelIndex(lines)
	return spanOver(lines, labels, chain)
}

// spanOver is maxJumpSpan's body over an already-scanned function, so the relay
// can re-measure without re-scanning.
//
// It walks the GOTOS rather than the labels: a label's own position is not what
// a jump reaches once chaining is in play, so every goto has to resolve its
// chain for itself.
func spanOver(lines []luaLine, labels map[string]int, chain map[string]string) funcSpan {
	var worst funcSpan
	for _, l := range lines {
		name, ok := gotoOn(l.text)
		if !ok {
			continue
		}
		final := resolveChain(name, chain)
		at, ok := labels[final]
		if !ok {
			continue
		}
		d := at - l.off
		if d < 0 {
			d = -d
		}
		if d > worst.bytes {
			worst = funcSpan{bytes: d, label: final}
		}
	}
	return worst
}

// luaLine is one line of emitted Lua with everything the scan and the relay need
// to decide about it.
//
// Indentation IS the block structure here, exactly: `builder.line` writes two
// spaces per `b.indent`, and every construct the emitter opens brackets its body
// with `b.indent++` / `b.indent--`. So a line at depth 1 is at FUNCTION BODY
// level -- the level `::L<n>::` and `goto L<n>` are written at -- and anything
// deeper is inside a block a body-level label may not be moved into.
type luaLine struct {
	off, end int    // byte offsets of the line and of the character past its '\n'
	depth    int    // leading spaces / 2
	text     string // trimmed
}

// scanLuaLines splits a function's emitted text, keeping EVERY line including
// blanks so the relay can reassemble the file byte for byte from the slices.
func scanLuaLines(src string) []luaLine {
	var out []luaLine
	off := 0
	for off < len(src) {
		end := len(src)
		if i := strings.IndexByte(src[off:], '\n'); i >= 0 {
			end = off + i + 1
		}
		raw := src[off:end]
		n := 0
		for n < len(raw) && raw[n] == ' ' {
			n++
		}
		out = append(out, luaLine{off: off, end: end, depth: n / 2, text: strings.TrimSpace(raw)})
		off = end
	}
	return out
}

// labelIndex records where every label sits and which label each one RELAYS to
// -- the chain described above maxJumpSpan.
func labelIndex(lines []luaLine) (labels map[string]int, chain map[string]string) {
	labels = map[string]int{}
	chain = map[string]string{}
	for i, l := range lines {
		name, ok := labelDefinedOn(l.text)
		if !ok {
			continue
		}
		labels[name] = l.off
		// What Lua sees next, skipping everything that emits no instruction.
		for j := i + 1; j < len(lines); j++ {
			t := lines[j].text
			if t == "" || strings.HasPrefix(t, "--") {
				continue
			}
			if _, isLabel := labelDefinedOn(t); isLabel {
				continue
			}
			if to, bare := bareGotoOn(t); bare {
				chain[name] = to
			}
			break
		}
	}
	return labels, chain
}

// resolveChain follows a relay chain to the label a jump really lands on.
//
// A cycle -- `::A:: goto A`, an infinite loop Lua compiles happily -- terminates
// on the visited set rather than spinning.
func resolveChain(name string, chain map[string]string) string {
	seen := map[string]bool{}
	for {
		next, ok := chain[name]
		if !ok || seen[name] {
			return name
		}
		seen[name] = true
		name = next
	}
}

// bareGotoOn reports the label of a statement that is NOTHING BUT a goto, which
// is the only shape that chains: anything else emits an instruction first and
// discharges the pending list.
func bareGotoOn(text string) (string, bool) {
	rest, ok := strings.CutPrefix(text, "goto ")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.ContainsAny(rest, " \t") {
		return "", false
	}
	return rest, true
}

// labelDefinedOn reports the label a line defines, if it defines one. The
// emitter writes a label on a line of its own: indentation, `::L<n>::`, end of
// line.
func labelDefinedOn(line string) (string, bool) {
	s := strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(s, "::")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(rest, "::")
	if !ok {
		return "", false
	}
	return name, true
}

// gotoOn reports the label a line jumps to, if it jumps.
//
// Three shapes reach here and all of them put the goto last on its line:
// `goto L3` on its own, `if <cond> then goto L3 end` from an `if` header or a
// simple br_if, and the same inside a branch-table chain.
func gotoOn(line string) (string, bool) {
	i := strings.Index(line, "goto ")
	if i < 0 {
		return "", false
	}
	rest := line[i+len("goto "):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// checkJumpSpan refuses a function whose widest jump would not fit in Lua's
// sBx field.
//
// The remedy names the OPTIMIZER rather than the author, because that is where
// the cause is. Nobody writes a function with a hundred thousand instructions
// in it; LLVM at -opt=2 inlines a whole program's worth of sections into one,
// and the fix is to tell it not to at the boundaries the author already thinks
// of as sections. Measured on the reproduction: twenty section functions of
// sixteen prototypes each are inlined into one whose jump crosses 1,556,741
// bytes, and the same source with //go:noinline on those twenty packages.
//
// THE SIZE CLAIM IS ATTRIBUTED AND NOT GENERAL, which is why the message says
// so in those words. The downstream guest that found this reported its module
// coming out 27% smaller once its six sections stopped being inlined; the
// reproduction here does not reproduce that -- 1,690,659 bytes inlined against
// 1,694,449 split, i.e. 0.2% the other way. Both are real; the win is a
// property of a particular guest's shape rather than a rule, and an error
// message that promised it would be wrong most of the time.
func checkJumpSpan(name string, src string) error {
	// A span cannot be longer than the text it sits in, so a function shorter
	// than the threshold needs no scan at all. That is every function in every
	// guest this repo has ever emitted -- the widest is 826 KB of chunk with a
	// 249 KB span in it -- which is what keeps this off the emitter's cost.
	if len(src) <= maxJumpSpanBytes {
		return nil
	}
	sp := maxJumpSpan(src)
	if sp.bytes <= maxJumpSpanBytes {
		return nil
	}
	return &JumpSpanError{Func: name, Bytes: sp.bytes, Label: sp.label}
}

// JumpSpanError is what a refusal for the jump limit is, as a type.
//
// Typed rather than an fmt.Errorf string because relayOrRefuse has to tell "this
// function would have been refused, try the relay" apart from any other failure,
// and a caller further out may want the same distinction without matching on
// prose. The message is the whole of the user-facing diagnostic and its wording
// is asserted by TestAnOverlongJumpSpanIsRefusedAtPackageTime.
type JumpSpanError struct {
	Func  string // the emitted function's name
	Bytes int    // the widest span in it, in bytes of generated Lua
	Label string // the label that span crosses to
}

func (e *JumpSpanError) Error() string {
	return fmt.Sprintf(
		"function %s is too big for Lua: one jump inside it crosses %d bytes of "+
			"generated Lua and the limit is about %d, because Lua 5.2 encodes a "+
			"jump offset in 18 bits and cannot express one longer than %d VM "+
			"instructions. Lua reports this at the player's game start as "+
			"\"control structure too long\", pointing into generated Lua and naming "+
			"nothing in your source. The emitter already tried to RELAY that jump "+
			"through a ladder of trampolines and could not: the whole span sits "+
			"inside one basic block, so there is nowhere at body level to put one. "+
			"Split the function: //go:noinline in Go, #[inline(never)] in Rust, on "+
			"the boundaries you already think of as sections. The optimizer inlined "+
			"a whole program into one function, and it is that concentration rather "+
			"than anything you wrote that reaches the limit. (One guest also came "+
			"out 27%% smaller for it; that is a property of its shape, not a rule.)",
		e.Func, e.Bytes, maxJumpSpanBytes, luaMaxJumpInstructions)
}

// ============================================================================
// The relay -- a too-long jump becomes a ladder of short ones
// ============================================================================
//
// THE IDEA IN ONE LINE: do not move the code, relay the jump. A jump that cannot
// be encoded is broken into hops that can, by inserting trampolines of the form
//
//	goto LTs3      <- the guard: the straight-line path steps over the station
//	::LT3::        <- the trampoline the previous hop lands on
//	if 0 ~= 0 then end
//	goto LT4       <- the next hop, or the original label
//	::LTs3::
//
// and pointing the original `goto` at the first one. Measured under bin/lua52f
// before any of this was written: a ten-hop ladder spanning 500,000 VM
// instructions -- 3.8x Lua's single-jump limit -- loads and computes the right
// answer (scratchpad/r4/RESULTS-jumplimit.txt).
//
// WHY THIS AND NOT THE OUTLINING PASS CLAUDE.md USED TO DESCRIBE. That design
// split the FUNCTION at a point where the operand stack and the control stack
// are both empty. Measured on the funclimit reproduction, 135,076 IR steps: four
// such points exist and ALL FOUR ARE OUTSIDE THE SPAN THE JUMP CROSSES. There is
// a theorem behind it -- a wasm branch targets an ENCLOSING construct, so the
// control stack is >= 1 at every step strictly inside any jump's span -- so a
// zero-control-depth split can never shorten the widest jump in a function. The
// relay runs on the EMITTED TEXT instead, which is why it renumbers nothing,
// invalidates no analysis result, adds no function, crosses no values and needs
// no size estimate: it measures the real bytes, which is the measurement this
// file already makes.
//
// WHAT IT COSTS. One statement per trampoline, executed only when the trampoline
// is ENTERED -- which for the dominant shape is a merged trap block, terminal and
// never hot -- plus one unconditional jump per station on the straight-line path,
// which is the guard. A 1.5 MB span needs four stations. Placing the stations
// where control cannot fall through instead would cost the straight-line path
// nothing at all and is measured to work; it is a refinement rather than a
// requirement, and the guarded form is what ships.

// relayHopBytes is how far one hop of the ladder may span.
//
// HALF the threshold, and the slack is deliberate rather than tuned. Stations
// are planned against the ORIGINAL offsets and each insertion pushes everything
// after it along by ~80 bytes, so the hops the emitted text really has are a
// little wider than the ones that were planned. Half of 655,355 leaves four
// orders of magnitude more room than that drift can use, at a cost of one extra
// station per 327 KB of span. relayJumps re-measures the result regardless, so
// this number is a margin and not a correctness argument.
func relayHopBytes() int { return maxJumpSpanBytes / 2 }

// relaySeparator is the ONE statement between a trampoline's label and its goto,
// and it is the whole reason the ladder is legal rather than a workaround.
//
// Without it the label and the jump concatenate their pending-jump lists and the
// entire ladder is patched as ONE jump to the final target -- see the chaining
// note above maxJumpSpan, where that is measured in both directions.
//
// IT NAMES NOTHING, and that is the requirement it was chosen for: a guest
// function may have no locals, no memory, no globals and no scratch registers,
// so any separator that referenced an emitter name would be one whose existence
// had to be proved per function. A comparison of two constants is not folded by
// Lua 5.2 -- `luaK_posfix` sends OPR_NE to `codecomp`, and `constfolding` is
// reached only from `codearith` -- so it really does emit an OP_EQ, and
// `luaK_code` discharges the pending list on the way past.
const relaySeparator = "if 0 ~= 0 then end"

// relayTrampolineName and relaySkipName are the two name families the relay
// adds. Both are indexed by a PER-FUNCTION trampoline counter, which is a dense
// small number and therefore exactly the hazard class the loop guard's `g%d`
// once was -- so each owns a prefix nothing else in the emitter uses. See
// agents/codegen.md's identifier table and nameFamilies in loopguard_test.go,
// which proves the disjointness rather than eyeballing the prefixes.
func relayTrampolineName(k int) string { return "LT" + strconv.Itoa(k) }
func relaySkipName(k int) string       { return "LTs" + strconv.Itoa(k) }

// relayStation is one planned trampoline: where it goes, what it is called and
// where it sends control next.
type relayStation struct {
	line  int    // the line index it is inserted BEFORE
	off   int    // that line's byte offset, which is what hops are measured in
	label string // the trampoline's own label
	skip  string // the guard label the straight-line path jumps to
	to    string // the label this hop jumps to
}

// relayOrRefuse is the shipping path: accept, or relay and accept, or refuse.
//
// The relay runs only on a function the check has already refused, so a guest
// under the threshold -- which is every guest this repo emits -- pays exactly
// what it paid before: one length comparison. TestEveryGuestThisRepoEmitsIsUnchangedByTheRelay
// asserts the stronger form, that the transform is a byte-for-byte no-op on the
// whole corpus even when called directly.
func relayOrRefuse(name, src string) (string, error) {
	err := checkJumpSpan(name, src)
	if err == nil {
		return src, nil
	}
	if out, ok := relayJumps(src); ok {
		if checkJumpSpan(name, out) == nil {
			return out, nil
		}
	}
	return "", err
}

// relayJumps breaks every over-long jump in one emitted function into hops.
//
// It reports whether it changed anything. A false return is not an error: it
// means either that nothing was over the threshold, or that the span could not
// be relayed -- a single basic block longer than the limit, where there is no
// body-level statement between the goto and its label to hang a trampoline on.
// The check is the backstop for that case and says so in its message.
//
// DETERMINISM. The transform is a pure function of the emitted text: labels are
// visited in order of first appearance rather than in map order, stations are
// planned by a greedy walk over line offsets, and the counter is per function.
// Two builds of the same module produce the same bytes.
func relayJumps(src string) (string, bool) {
	lines := scanLuaLines(src)
	labels, chain := labelIndex(lines)
	if spanOver(lines, labels, chain).bytes <= maxJumpSpanBytes {
		return src, false
	}

	pts := insertionPoints(lines)
	if len(pts) == 0 {
		return src, false
	}

	// Every goto, grouped by the label it really reaches, in order of first
	// appearance so the plan does not depend on Go's map iteration order.
	byLabel := map[string][]int{}
	var order []string
	for i, l := range lines {
		name, ok := gotoOn(l.text)
		if !ok {
			continue
		}
		final := resolveChain(name, chain)
		if _, ok := labels[final]; !ok {
			continue
		}
		if _, seen := byLabel[final]; !seen {
			order = append(order, final)
		}
		byLabel[final] = append(byLabel[final], i)
	}

	hop := relayHopBytes()
	next := 0 // the per-function trampoline counter
	var stations []relayStation
	rewrite := map[int]string{} // line index -> the label its goto should name

	for _, name := range order {
		at := labels[name]
		// A trampoline goes at BODY level, so it can only relay a jump whose
		// label is at body level too: a goto may leave a block but never enter
		// one. Every label the emitter writes is at body level; a label that is
		// not simply goes unrelayed and the check refuses the function.
		if labelDepth(lines, name) != 1 {
			continue
		}

		var fwd, back []int
		for _, i := range byLabel[name] {
			if lines[i].off < at {
				fwd = append(fwd, i)
			} else {
				back = append(back, i)
			}
		}

		// FORWARD. The widest is the earliest goto, so the ladder is planned
		// from it and every other forward goto joins the ladder at the first
		// station past its own position -- which is within one hop by
		// construction, since the stations are at most one hop apart.
		if len(fwd) > 0 {
			first := lines[fwd[0]].off
			for _, i := range fwd {
				if lines[i].off < first {
					first = lines[i].off
				}
			}
			if at-first > hop {
				var plan []int
				cur, ok := first, true
				for at-cur > hop {
					p := lastPointIn(pts, cur+1, cur+hop)
					if p < 0 {
						ok = false
						break
					}
					plan = append(plan, p)
					cur = p
				}
				if ok {
					base := next
					next += len(plan)
					for k, p := range plan {
						to := name
						if k+1 < len(plan) {
							to = relayTrampolineName(base + k + 1)
						}
						stations = append(stations, relayStation{
							line: lineAt(lines, p), off: p,
							label: relayTrampolineName(base + k),
							skip:  relaySkipName(base + k),
							to:    to,
						})
					}
					for _, i := range fwd {
						for k, p := range plan {
							if p > lines[i].off {
								rewrite[i] = relayTrampolineName(base + k)
								break
							}
						}
					}
				}
			}
		}

		// BACKWARD, which is the same ladder with the sign flipped. Lua's own
		// test is abs(offset), so a loop back edge over an enormous body is the
		// same defect from the other end and gets the same remedy.
		if len(back) > 0 {
			last := lines[back[0]].off
			for _, i := range back {
				if lines[i].off > last {
					last = lines[i].off
				}
			}
			if last-at > hop {
				var plan []int
				cur, ok := last, true
				for cur-at > hop {
					p := firstPointIn(pts, cur-hop, cur-1)
					if p < 0 || p <= at {
						ok = false
						break
					}
					plan = append(plan, p)
					cur = p
				}
				if ok {
					base := next
					next += len(plan)
					for k, p := range plan {
						to := name
						if k+1 < len(plan) {
							to = relayTrampolineName(base + k + 1)
						}
						stations = append(stations, relayStation{
							line: lineAt(lines, p), off: p,
							label: relayTrampolineName(base + k),
							skip:  relaySkipName(base + k),
							to:    to,
						})
					}
					// plan runs from the goto BACK towards the label, so it is
					// in decreasing offset order and the nearest station above
					// a given goto is the first entry below it.
					for _, i := range back {
						for k, p := range plan {
							if p < lines[i].off {
								rewrite[i] = relayTrampolineName(base + k)
								break
							}
						}
					}
				}
			}
		}
	}

	if len(stations) == 0 {
		return src, false
	}

	at := map[int][]relayStation{}
	for _, st := range stations {
		at[st.line] = append(at[st.line], st)
	}

	var b strings.Builder
	b.Grow(len(src) + len(stations)*96)
	for i, l := range lines {
		for _, st := range at[i] {
			b.WriteString("  goto " + st.skip + "\n")
			b.WriteString("  ::" + st.label + "::\n")
			b.WriteString("  " + relaySeparator + "\n")
			b.WriteString("  goto " + st.to + "\n")
			b.WriteString("  ::" + st.skip + "::\n")
		}
		raw := src[l.off:l.end]
		if to, ok := rewrite[i]; ok {
			raw = rewriteGoto(raw, to)
		}
		b.WriteString(raw)
	}
	return b.String(), true
}

// insertionPoints is every byte offset a trampoline may be inserted before.
//
// Three conditions, and each of them is a way the transform could otherwise
// produce Lua that does not parse or does not mean what it says:
//
//   - DEPTH EXACTLY 1. That is function-body level, where the emitter writes its
//     own labels. A trampoline deeper than that would be a label inside a block,
//     and a goto may leave a block but never enter one.
//   - NOT A CLOSER. `end`, `else`, `elseif` and `until` continue a construct
//     rather than starting a statement, so inserting before one puts the
//     trampoline inside the block being closed. A line that OPENS a block is a
//     fine boundary -- the insertion goes before the whole construct.
//   - AFTER THE PROLOGUE, and after a preceding line that finishes its
//     statement. The first is Invariant B: every local is declared in one run at
//     the top, and a trampoline between two of them would relay a jump INTO a
//     local's scope, which Lua rejects with "jumps into the scope of local". The
//     second catches a statement written across two lines at one depth, where
//     the second line is not a boundary at all.
//
// The prologue rule is also where Invariant B is CHECKED rather than assumed:
// measured on examples/api, bench/guests/go and examples/gcbench at -opt=3,
// there is not one body-level `local` after a label in any function, so the
// prologue really is one run and everything after it is fair game.
func insertionPoints(lines []luaLine) []int {
	prologue := 0
	for i, l := range lines {
		if l.depth == 1 && strings.HasPrefix(l.text, "local ") {
			prologue = i + 1
		}
	}
	var out []int
	for i := prologue; i < len(lines); i++ {
		l := lines[i]
		if l.depth != 1 || l.text == "" || strings.HasPrefix(l.text, "--") {
			continue
		}
		if isBlockCloser(l.text) {
			continue
		}
		p := previousCodeLine(lines, i)
		if p < 0 || lines[p].depth < 1 || !endsStatement(lines[p].text) {
			continue
		}
		out = append(out, l.off)
	}
	return out
}

// isBlockCloser reports whether a line continues an open construct rather than
// starting a statement.
func isBlockCloser(text string) bool {
	for _, w := range [...]string{"end", "else", "elseif", "until"} {
		if text == w || strings.HasPrefix(text, w+" ") || strings.HasPrefix(text, w+"(") {
			return true
		}
	}
	return false
}

// previousCodeLine is the nearest line above i that emits something -- blanks
// and comments are neither statements nor boundaries.
func previousCodeLine(lines []luaLine, i int) int {
	for j := i - 1; j >= 0; j-- {
		if t := lines[j].text; t != "" && !strings.HasPrefix(t, "--") {
			return j
		}
	}
	return -1
}

// endsStatement reports whether a line finishes what it started, so that the gap
// after it is a statement boundary.
//
// Stated as a blacklist of trailing tokens rather than a whitelist: the emitter
// writes one statement per builder.line today, so the list is a guard against a
// future lowering that spreads one over two lines at the same depth, and a guard
// wants to fail closed on anything it does not recognise.
func endsStatement(text string) bool {
	for _, s := range [...]string{",", "(", "{", "=", "..", "+", "-", "*", "/",
		"%", "^", "#", "<", ">", "~", " and", " or", " not", " then", " do", " else"} {
		if strings.HasSuffix(text, s) {
			return false
		}
	}
	return text != ""
}

// lastPointIn is the largest insertion point in [lo, hi], or -1.
func lastPointIn(pts []int, lo, hi int) int {
	best := -1
	for _, p := range pts {
		if p < lo {
			continue
		}
		if p > hi {
			break
		}
		best = p
	}
	return best
}

// firstPointIn is the smallest insertion point in [lo, hi], or -1.
func firstPointIn(pts []int, lo, hi int) int {
	for _, p := range pts {
		if p < lo {
			continue
		}
		if p > hi {
			return -1
		}
		return p
	}
	return -1
}

// lineAt is the index of the line beginning at offset off.
func lineAt(lines []luaLine, off int) int {
	for i, l := range lines {
		if l.off == off {
			return i
		}
	}
	return -1
}

// labelDepth is the indentation depth of a label's definition, or -1.
func labelDepth(lines []luaLine, name string) int {
	for _, l := range lines {
		if n, ok := labelDefinedOn(l.text); ok && n == name {
			return l.depth
		}
	}
	return -1
}

// rewriteGoto points one `goto` at a different label, leaving the rest of the
// line -- indentation, an `if ... then` head, the trailing `end` -- alone.
func rewriteGoto(raw, to string) string {
	i := strings.Index(raw, "goto ")
	if i < 0 {
		return raw
	}
	j := i + len("goto ")
	k := j
	for k < len(raw) && raw[k] != ' ' && raw[k] != '\n' {
		k++
	}
	return raw[:j] + to + raw[k:]
}
