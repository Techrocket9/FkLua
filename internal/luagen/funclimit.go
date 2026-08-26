package luagen

import (
	"fmt"
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
// WHAT IT LOOKS LIKE WHEN IT FIRES, and it names nothing useful. The engine
// reports the token the parser was holding when the pending gotos were patched,
// which is whatever follows the label:
//
//	control structure too long near 'trap_unreachable'
//
// It names no file, no function and nothing about the guest -- so an author
// gets a mod that refuses to load and no route back to the Go or Rust that
// caused it. That is the whole reason this check exists, and it is the same
// argument checkChunkLocals is built on one limit over.
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
func maxJumpSpan(src string) funcSpan {
	// Where each label is defined, and where each goto to it sits. A label is
	// defined once; a label can be jumped to many times, and only the widest
	// of those matters.
	type site struct{ first, last int }
	labels := map[string]int{}
	gotos := map[string]site{}

	off := 0
	for len(src) > 0 {
		line := src
		next := len(src)
		if i := strings.IndexByte(src, '\n'); i >= 0 {
			line, next = src[:i], i+1
		}
		if name, ok := labelDefinedOn(line); ok {
			labels[name] = off
		}
		if name, ok := gotoOn(line); ok {
			s, seen := gotos[name]
			if !seen {
				s.first = off
			}
			s.last = off
			gotos[name] = s
		}
		off += next
		src = src[next:]
	}

	var worst funcSpan
	for name, at := range labels {
		g, ok := gotos[name]
		if !ok {
			continue
		}
		for _, from := range [2]int{g.first, g.last} {
			d := at - from
			if d < 0 {
				d = -d
			}
			if d > worst.bytes {
				worst = funcSpan{bytes: d, label: name}
			}
		}
	}
	return worst
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
	return fmt.Errorf(
		"function %s is too big for Lua: one jump inside it crosses %d bytes of "+
			"generated Lua and the limit is about %d, because Lua 5.2 encodes a "+
			"jump offset in 18 bits and cannot express one longer than %d VM "+
			"instructions. Lua reports this at the player's game start as "+
			"\"control structure too long\", naming neither the file nor the "+
			"function that caused it. Split the function: //go:noinline in Go, "+
			"#[inline(never)] in Rust, on the boundaries you already think of as "+
			"sections. The optimizer inlined a whole program into one function, "+
			"and it is that concentration rather than anything you wrote that "+
			"reaches the limit. (One guest also came out 27%% smaller for it; "+
			"that is a property of its shape, not a rule.)",
		name, sp.bytes, maxJumpSpanBytes, luaMaxJumpInstructions)
}
