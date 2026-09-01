package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// spanModule is a handful of functions with nothing subtle in them: what the
// spans have to describe is where each one STARTS and STOPS, and that question
// does not get easier or harder with the body's contents.
const spanModule = `(module (memory 1)
  (func $one (result i32) (i32.const 1))
  (func $two (param i32) (result i32) (local.get 0) (i32.const 2) (i32.add))
  (func $three (export "fk_on_tick") (param i32)
    (block (loop (br_if 1 (local.get 0)) (br 0))))
  (func $four (result f64) (f64.const 1.5)))`

func spansFor(t *testing.T, src string, opts Options) (string, []FuncSpan) {
	t.Helper()
	m, err := wasm.DecodeWAT(src)
	if err != nil {
		t.Fatalf("wat: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	out, spans, err := EmitModuleSpans(im, opts)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return out, spans
}

// THE RANGE HAS TO CONTAIN THE FUNCTION, which is the whole claim a debug map
// rests on: every consumer of one takes a line number, finds the range that
// holds it and reports that function's name. An off-by-one attributes a frame
// to the function next door and never says so.
//
// Asserted against the emitted text rather than against remembered numbers: the
// first line of a span is the banner comment naming the function, and the last
// is the line that closes its body.
func TestASpanBracketsItsFunction(t *testing.T) {
	src, spans := spansFor(t, spanModule, Options{})
	lines := strings.Split(src, "\n")
	if len(spans) != 4 {
		t.Fatalf("4 functions, %d spans", len(spans))
	}
	for i, s := range spans {
		if s.Start < 1 || s.End > len(lines) || s.End < s.Start {
			t.Fatalf("%s: range [%d, %d] is not inside a %d-line chunk",
				s.Name, s.Start, s.End, len(lines))
		}
		if banner := lines[s.Start-1]; !strings.HasPrefix(banner, "-- "+s.Name+" ") {
			t.Errorf("%s starts at line %d, which is %q", s.Name, s.Start, banner)
		}
		if last := strings.TrimSpace(lines[s.End-1]); last != "end" {
			t.Errorf("%s ends at line %d, which is %q", s.Name, s.End, last)
		}
		// The blank line the emitter writes between two functions belongs to
		// neither of them. A span that swallowed it would still bracket its
		// function and would still be wrong, so the separator is checked
		// explicitly -- from the second function on, since the first one
		// follows the module's own header rather than a separator.
		if i > 0 && strings.TrimSpace(lines[s.Start-2]) != "" {
			t.Errorf("%s does not start at the top of its block: %q",
				s.Name, lines[s.Start-2])
		}
	}
}

// Ascending, disjoint, and one entry per DEFINED function in wasm index order.
// A consumer binary-searches these, which is only meaningful if all three hold.
func TestSpansAreOrderedAndDisjoint(t *testing.T) {
	m, err := wasm.DecodeWAT(spanModule)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	_, spans, err := EmitModuleSpans(im, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != len(im.Funcs) {
		t.Fatalf("%d functions emitted, %d spans", len(im.Funcs), len(spans))
	}
	prev := 0
	for i, s := range spans {
		if s.Index != im.Funcs[i].Index {
			t.Errorf("span %d is function %d, want %d", i, s.Index, im.Funcs[i].Index)
		}
		if s.Start <= prev {
			t.Errorf("span %d starts at %d, inside or before the previous one (ends %d)",
				i, s.Start, prev)
		}
		prev = s.End
	}
}

// The spans are the same chunk, whatever the level: -opt and --persist change
// what a body contains and never which bodies there are or what order they come
// in. Ranges move; the bracketing property does not.
func TestSpansHoldAtEveryLevel(t *testing.T) {
	for _, lvl := range []analysis.Level{analysis.O0, analysis.O1, analysis.O2, analysis.O3} {
		for _, p := range []PersistMode{PersistNone, PersistTable, PersistPacked} {
			name := fmt.Sprintf("%s-%s", lvl, p)
			t.Run(name, func(t *testing.T) {
				src, spans := spansFor(t, spanModule, Options{Opt: lvl, Persist: p})
				lines := strings.Split(src, "\n")
				for _, s := range spans {
					if !strings.HasPrefix(lines[s.Start-1], "-- "+s.Name+" ") {
						t.Errorf("%s: line %d is %q", s.Name, s.Start, lines[s.Start-1])
					}
					if strings.TrimSpace(lines[s.End-1]) != "end" {
						t.Errorf("%s: line %d is %q", s.Name, s.End, lines[s.End-1])
					}
				}
			})
		}
	}
}

// A RELAYED FUNCTION MOVES EVERY FUNCTION BELOW IT, and the spans have to
// follow. This is the case the whole delta accounting exists for: the relay
// rewrites one body in place, inserting a ladder of trampoline lines, so a
// range measured before it runs is correct for that function and wrong for
// every one after.
//
// The threshold is lowered rather than the fixture grown, so the relay fires on
// a module small enough to assert against line by line.
func TestSpansSurviveTheRelay(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 3000; i++ {
		body.WriteString(" (local.set 1 (i32.add (local.get 1) (i32.const 1)))")
	}
	// A relayed function, and a small one AFTER it whose range is the thing at
	// risk.
	src := `(module (memory 1)
  (func $big (export "big") (param i32) (result i32) (local i32)
    (block $out
      (br_if $out (local.get 0))` + body.String() + `
      (return (local.get 1)))
    (unreachable))
  (func $after (result i32) (i32.const 7)))`

	saved := maxJumpSpanBytes
	t.Cleanup(func() { maxJumpSpanBytes = saved })

	maxJumpSpanBytes = 1 << 30
	plain, plainSpans := spansFor(t, src, Options{})
	maxJumpSpanBytes = 20000
	relayed, spans := spansFor(t, src, Options{})
	maxJumpSpanBytes = saved

	if plain == relayed {
		t.Fatal("the two arms are byte-identical, so the relay did not run and " +
			"there is nothing here to check")
	}
	if plainSpans[1].Start == spans[1].Start {
		t.Fatal("the relay inserted nothing above the second function, so this " +
			"does not test that the spans followed it")
	}
	lines := strings.Split(relayed, "\n")
	for _, s := range spans {
		if !strings.HasPrefix(lines[s.Start-1], "-- "+s.Name+" ") {
			t.Errorf("%s claims line %d, which is %q", s.Name, s.Start, lines[s.Start-1])
		}
		if got := strings.TrimSpace(lines[s.End-1]); got != "end" {
			t.Errorf("%s ends at line %d, which is %q", s.Name, s.End, got)
		}
	}
	// The relayed function's own range grew by exactly what was inserted into
	// it, which is the other half: a span that kept its old END would stop
	// short of its own body.
	grew := (spans[0].End - spans[0].Start) - (plainSpans[0].End - plainSpans[0].Start)
	if grew <= 0 {
		t.Errorf("the relayed function's range grew by %d lines", grew)
	}
}

// EmitModuleWith is the same function with one result dropped, and the seventy
// call sites that use it have to keep getting the chunk they always got.
func TestTheTwoEntryPointsEmitTheSameChunk(t *testing.T) {
	m, err := wasm.DecodeWAT(spanModule)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	a, err := EmitModuleWith(im, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := EmitModuleSpans(im, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("EmitModuleWith and EmitModuleSpans disagree about the chunk")
	}
}
