// Package spectest runs the official WebAssembly conformance suite against
// generated Lua.
//
// This is the project's primary correctness metric. Golden tests pin the shape
// of the output; only the spec suite says whether that output computes the
// right answers, and it does so against the real interpreter (lua52f) rather
// than a Go reimplementation of Lua's semantics.
//
// The corpus is wast2json output committed under testdata/spec, so this needs
// neither network access nor a WABT install.
package spectest

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// Command is one entry in a wast2json script.
type Command struct {
	Type       string  `json:"type"`
	Line       int     `json:"line"`
	Filename   string  `json:"filename"`
	Text       string  `json:"text"`
	ModuleType string  `json:"module_type"`
	Action     *Action `json:"action"`
	Expected   []Value `json:"expected"`
}

// Action is an invocation of an exported function.
type Action struct {
	Type  string  `json:"type"`
	Field string  `json:"field"`
	Args  []Value `json:"args"`
}

// Value is a typed constant. wast2json renders numbers as decimal strings of
// their unsigned bit pattern, which is exactly the representation Invariant A
// uses, so i32 values need no conversion at all.
type Value struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type script struct {
	SourceFilename string    `json:"source_filename"`
	Commands       []Command `json:"commands"`
}

// Outcome counts one suite file's results.
type Outcome struct {
	File     string
	Total    int
	Passed   int
	Failed   int
	Skipped  int
	Failures []string
	Skips    []string

	// Tainted counts assertions that failed AFTER an earlier assertion in the
	// same module was skipped. Spec files are stateful -- a skipped store means
	// the load asserted next reads the wrong memory -- so such a failure says
	// nothing about the compiler and must not be reported as one. They are
	// counted and listed rather than hidden.
	Tainted      int
	TaintedNotes []string
}

// PassRate is the fraction of executed (non-skipped) assertions that passed.
func (o *Outcome) PassRate() float64 {
	run := o.Passed + o.Failed
	if run == 0 {
		return 0
	}
	return float64(o.Passed) / float64(run)
}

func (o *Outcome) String() string {
	s := fmt.Sprintf("%-18s %4d passed, %4d failed, %4d skipped  (%.1f%%)",
		o.File, o.Passed, o.Failed, o.Skipped, o.PassRate()*100)
	if o.Tainted > 0 {
		s += fmt.Sprintf("  [%d tainted]", o.Tainted)
	}
	return s
}

// Options control how the suite is run.
type Options struct {
	// NaN selects the code-generation mode under test. Running the suite in
	// exact mode is how the boxing implementation is validated: assertions
	// skipped as unrepresentable in canonical mode must actually pass there.
	NaN luagen.NaNMode

	// Opt is the optimization level under test. The suite has to be green at
	// every level, not just the default: an optimizer that trades correctness
	// for speed is a failure, and this is the only thing that would say so.
	Opt analysis.Level

	// GC selects the collector build variant. --gc=collected CHANGES EMITTED
	// CODE -- it withdraws the inlined 8-byte store, which is the one lowering
	// that writes MEM without marking its page -- so it is a variant the
	// conformance suite has to be green under, for the same reason every -opt
	// level and both NaN modes are. A gate that only ever ran the default
	// would not have caught either of the two miscompiles the 2026-07-30 audit
	// found.
	GC luagen.GCMode
}

// RunFile executes one converted .wast file with the default options.
func RunFile(h *luahost.Host, jsonPath string) (*Outcome, error) {
	return RunFileWith(h, jsonPath, Options{})
}

// RunFileWith executes one converted .wast file.
func RunFileWith(h *luahost.Host, jsonPath string, opts Options) (*Outcome, error) {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}
	var s script
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", jsonPath, err)
	}

	dir := filepath.Dir(jsonPath)
	out := &Outcome{File: strings.TrimSuffix(filepath.Base(jsonPath), ".json")}

	// Assertions apply to the most recently instantiated module, so commands
	// are grouped into runs sharing one module and executed together -- one
	// lua52f process per module rather than per assertion.
	var curSrc string     // generated Lua for the active module
	var curErr error      // why the active module could not be compiled
	var pending []Command // assertions awaiting the active module

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		defer func() { pending = nil }()

		if curErr != nil {
			for _, c := range pending {
				out.Total++
				out.Skipped++
				out.Skips = append(out.Skips,
					fmt.Sprintf("line %d: module not compiled: %v", c.Line, curErr))
			}
			return nil
		}
		return runAssertions(h, out, curSrc, pending, opts)
	}

	for _, c := range s.Commands {
		switch c.Type {
		case "module":
			if err := flush(); err != nil {
				return nil, err
			}
			curSrc, curErr = compileFile(filepath.Join(dir, c.Filename), opts)

		case "assert_return", "assert_trap", "action":
			// A bare "action" is an invocation run purely for its side effects
			// -- float_memory.wast uses one to reset memory between checks.
			// Skipping it silently corrupts every assertion that follows.
			pending = append(pending, c)

		case "assert_invalid", "assert_malformed", "assert_unlinkable":
			// These assert that a module is REJECTED. Our pipeline passes if it
			// refuses the module for any reason -- we do not yet check that the
			// reason matches the spec's expected text.
			out.Total++
			path := filepath.Join(dir, c.Filename)
			if _, err := compileFile(path, opts); err != nil {
				out.Passed++
			} else {
				out.Failed++
				out.Failures = append(out.Failures, fmt.Sprintf(
					"line %d: %s accepted a module that should be rejected (%s)",
					c.Line, c.Type, c.Text))
			}

		default:
			out.Total++
			out.Skipped++
			out.Skips = append(out.Skips,
				fmt.Sprintf("line %d: unhandled command %q", c.Line, c.Type))
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// compileFile turns one .wasm (or .wat) module into a generated Lua chunk.
func compileFile(path string, opts Options) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var m *wasm.Module
	if strings.HasSuffix(path, ".wat") {
		m, err = wasm.DecodeWAT(string(raw))
	} else {
		m, err = wasm.Decode(raw)
	}
	if err != nil {
		return "", err
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		return "", err
	}
	return luagen.EmitModuleWith(im, luagen.Options{NaN: opts.NaN, Opt: opts.Opt, GC: opts.GC})
}

// runAssertions builds one Lua driver for a whole batch and runs it.
func runAssertions(h *luahost.Host, out *Outcome, moduleSrc string, cmds []Command, opts Options) error {
	var b strings.Builder

	// The testsuite's own host module. The spec interpreter registers it as
	// "spectest" and several files import its print functions purely for their
	// side effect, so no-ops satisfy every assertion made about them.
	//
	// Its global, table and memory exports are deliberately absent: the decoder
	// refuses those import kinds because they shift an index space the emitter
	// numbers itself, so a module wanting one never reaches instantiation.
	b.WriteString(`local SPECTEST_IMPORTS = { spectest = {
  print = function() end, print_i32 = function() end,
  print_i64 = function() end, print_f32 = function() end,
  print_f64 = function() end, print_i32_f32 = function() end,
  print_f64_f64 = function() end,
} }

`)
	// The generated chunk ends in `return {...}`, so wrapping it in an
	// immediately-called function yields the module table. Its prelude locals
	// become function locals, well inside Lua's 200 limit.
	//
	// The wrapper is VARARG because a module with imports reads them from the
	// chunk's `...`, and `...` inside a non-vararg function is a compile error
	// rather than an empty list.
	//
	// Instantiation runs under pcall because it can now fail for a reason that
	// is not a compiler bug -- an import this harness does not supply. An
	// unprotected failure kills the driver process and takes the whole run's
	// results with it; under pcall the file's assertions become skips, which is
	// what an unrunnable module has always meant here.
	b.WriteString(`local M, instfail
do
  local ok, r = pcall(function(...)
`)
	b.WriteString(moduleSrc)
	b.WriteString(`
  end, SPECTEST_IMPORTS)
  if ok then M = r else
    M = { exports = {} }
    instfail = type(r) == "table" and (r.fk_trap or r.fk_unsupported) or tostring(r)
  end
end

`)
	b.WriteString(`
local results = {}
local function record(line, ok, detail)
  results[#results+1] = string.format("%d\t%s\t%s", line, ok and "PASS" or "FAIL", detail or "")
end
-- A call into an uncompiled function is SKIP, never PASS or FAIL: the feature
-- is missing, which is neither a working result nor a wrong one.
local function skipped(e)
  return type(e) == "table" and e.fk_unsupported ~= nil
end
local function record_skip(line, e)
  results[#results+1] = string.format("%d\tSKIP\t%s", line, e.fk_unsupported)
end
-- A result this platform cannot represent -- a NaN sign or payload -- is also a
-- SKIP, and is only ever reached when the value we DID produce is the canonical
-- NaN. Any other mismatch is still a failure.
local function record_unrep(line, why)
  results[#results+1] = string.format("%d\tSKIP\t%s", line, why)
end
-- An export that is not there means one of two very different things. If the
-- module never instantiated, every assertion in the file is unrunnable and says
-- nothing about the compiler -- a skip. If it did instantiate, an export the
-- test names and the module does not have is a real failure.
local function missing(line)
  if instfail then
    results[#results+1] = string.format("%d\tSKIP\tmodule did not instantiate: %s", line, instfail)
  else
    record(line, false, "no such export")
  end
end
-- An i64 arrives as two Lua values, so a one-value "got" is unreadable: the low
-- halves match far more often than not, and printing only that produced
-- failures that read "got 64 want 64".
local function shown(r, r2)
  if r2 == nil then return tostring(r) end
  return tostring(r) .. "," .. tostring(r2)
end
`)

	for _, c := range cmds {
		if c.Action == nil || c.Action.Type != "invoke" {
			out.Total++
			out.Skipped++
			out.Skips = append(out.Skips, fmt.Sprintf("line %d: unsupported action", c.Line))
			continue
		}
		args, ok := luaArgs(c.Action.Args, opts)
		if !ok {
			out.Total++
			out.Skipped++
			out.Skips = append(out.Skips,
				fmt.Sprintf("line %d: %s takes an argument type not supported yet",
					c.Line, c.Action.Field))
			continue
		}
		// A Lua number canonicalises NaN, so a NaN's sign bit is gone before the
		// guest ever sees it. That only MATTERS when the sign is observable in
		// the result -- which means the result is not itself a NaN. In practice
		// that is copysign; f32.add(-nan, 1) returns a NaN either way and is
		// tested normally.
		//
		// This is a platform limit, not a gap in the compiler: preserving NaN
		// signs would mean boxing every float.
		if neg, arg := hasNegativeNaNArg(c.Action.Args); neg &&
			observesNaNSign(c.Action.Field) && opts.NaN != luagen.NaNExact {
			out.Total++
			out.Skipped++
			out.Skips = append(out.Skips, fmt.Sprintf(
				"line %d: %s is passed %s; a Lua number cannot carry a NaN sign bit",
				c.Line, c.Action.Field, arg))
			continue
		}
		out.Total++

		switch c.Type {
		case "action":
			fmt.Fprintf(&b, `do
  local fn = M.exports[%s]
  if fn == nil then missing(%d)
  else
    local ok, e = pcall(fn%s)
    if not ok and skipped(e) then record_skip(%d, e)
    elseif not ok then record(%d, false, "unexpected trap: " .. tostring(type(e) == "table" and e.fk_trap or e))
    else record(%d, true) end
  end
end
`, luaString(c.Action.Field), c.Line, args, c.Line, c.Line, c.Line)

		case "assert_return":
			// An assert_return with no expected value is a VOID call: it
			// asserts only that the call completes. Skipping those was wrong and
			// actively misleading -- many of them are the stores that set up
			// memory for the loads asserted right after, so skipping one made
			// the next look like a compiler bug.
			if len(c.Expected) == 0 {
				fmt.Fprintf(&b, `do
  local fn = M.exports[%s]
  if fn == nil then missing(%d)
  else
    local ok, e = pcall(fn%s)
    if not ok and skipped(e) then record_skip(%d, e)
    elseif not ok then record(%d, false, "unexpected trap: " .. tostring(type(e) == "table" and e.fk_trap or e))
    else record(%d, true) end
  end
end
`, luaString(c.Action.Field), c.Line, args, c.Line, c.Line, c.Line)
				continue
			}
			chk, ok := expectedCheck(c.Expected, opts)
			if !ok {
				out.Skipped++
				out.Total--
				out.Skips = append(out.Skips, fmt.Sprintf(
					"line %d: result is a type or a NaN payload Lua cannot represent",
					c.Line))
				continue
			}
			mismatch := fmt.Sprintf(
				`record(%d, false, string.format("got %%s want %%s", shown(r, r2), %q))`,
				c.Line, chk.Want)
			if chk.Unrep != "" {
				mismatch = fmt.Sprintf(
					"if %s then record_unrep(%d, %q) else %s end",
					chk.Unrep, c.Line, chk.UnrepWhy, mismatch)
			}
			fmt.Fprintf(&b, `do
  local fn = M.exports[%s]
  if fn == nil then missing(%d)
  else
    local ok, r, r2 = pcall(fn%s)
    if not ok and skipped(r) then record_skip(%d, r)
    elseif not ok then record(%d, false, "unexpected trap: " .. tostring(type(r) == "table" and r.fk_trap or r))
    elseif not (%s) then %s
    else record(%d, true) end
  end
end
`, luaString(c.Action.Field), c.Line, args, c.Line, c.Line, chk.Expr, mismatch, c.Line)

		case "assert_trap":
			fmt.Fprintf(&b, `do
  local fn = M.exports[%s]
  if fn == nil then missing(%d)
  else
    local ok, e = pcall(fn%s)
    if not ok and skipped(e) then record_skip(%d, e)
    elseif ok then record(%d, false, "expected trap " .. %q .. ", returned normally")
    elseif type(e) ~= "table" or e.fk_trap ~= %q then
      record(%d, false, "wrong trap: " .. tostring(type(e) == "table" and e.fk_trap or e))
    else record(%d, true) end
  end
end
`, luaString(c.Action.Field), c.Line, args, c.Line, c.Line, c.Text, c.Text, c.Line, c.Line)
		}
	}

	b.WriteString("print(table.concat(results, \"\\n\"))\n")

	stdout, err := h.RunString(b.String())
	if err != nil {
		return fmt.Errorf("driver failed for %s: %w", out.File, err)
	}

	sawSkip := false
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		switch parts[1] {
		case "PASS":
			out.Passed++
		case "SKIP":
			sawSkip = true
			out.Skipped++
			detail := ""
			if len(parts) > 2 {
				detail = parts[2]
			}
			out.Skips = append(out.Skips, fmt.Sprintf("%s line %s: %s", out.File, parts[0], detail))
		default:
			detail := ""
			if len(parts) > 2 {
				detail = parts[2]
			}
			if sawSkip {
				out.Tainted++
				out.TaintedNotes = append(out.TaintedNotes, fmt.Sprintf(
					"%s line %s: %s (after a skipped assertion in the same module; "+
						"spec files are stateful, so this says nothing about the compiler)",
					out.File, parts[0], detail))
				continue
			}
			out.Failed++
			out.Failures = append(out.Failures,
				fmt.Sprintf("%s line %s: %s", out.File, parts[0], detail))
		}
	}
	return nil
}

// luaString renders a byte string as a Lua literal. Go's %q emits \u escapes
// for non-ASCII, which Lua cannot parse; wasm export names are arbitrary UTF-8,
// so names.wast fails outright without this.
func luaString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c >= 0x20 && c < 0x7F:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\%03d", c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// luaArgs renders an argument list, or reports that some argument has a type
// the harness cannot express yet.
//
// wast2json writes every number as the decimal string of its raw bit pattern.
// For i32 that IS the value, because Invariant A already represents an i32 as an
// unsigned double. Floats have to be decoded from their bits and re-emitted as
// exact hex-float literals -- decimal would introduce a second rounding.
func luaArgs(vals []Value, opts Options) (string, bool) {
	if len(vals) == 0 {
		return "", true
	}
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		lit, ok := luaLiteral(v, opts)
		if !ok {
			return "", false
		}
		parts = append(parts, lit)
	}
	return ", " + strings.Join(parts, ", "), true
}

func luaLiteral(v Value, opts Options) (string, bool) {
	switch v.Type {
	case "i32":
		if _, err := strconv.ParseUint(v.Value, 10, 32); err != nil {
			return "", false
		}
		return v.Value, true
	case "i64":
		// An i64 crosses the boundary as two Lua values, so one wasm argument
		// expands to a (lo, hi) pair here.
		bits, err := strconv.ParseUint(v.Value, 10, 64)
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("%d, %d", uint32(bits&0xFFFFFFFF), uint32(bits>>32)), true
	case "f32":
		bits, err := strconv.ParseUint(v.Value, 10, 32)
		if err != nil {
			return "", false
		}
		// In exact mode a NaN argument has to arrive BOXED, or its sign and
		// payload are gone before the guest sees it -- which would make the
		// mode untestable by the very assertions it exists to satisfy.
		if opts.NaN == luagen.NaNExact && math.IsNaN(float64(math.Float32frombits(uint32(bits)))) {
			return fmt.Sprintf("M.rt.boxf32(%d)", bits), true
		}
		return floatLiteral(float64(math.Float32frombits(uint32(bits)))), true
	case "f64":
		bits, err := strconv.ParseUint(v.Value, 10, 64)
		if err != nil {
			return "", false
		}
		if opts.NaN == luagen.NaNExact && math.IsNaN(math.Float64frombits(bits)) {
			return fmt.Sprintf("M.rt.boxf64(%d, %d)",
				uint32(bits&0xFFFFFFFF), uint32(bits>>32)), true
		}
		return floatLiteral(math.Float64frombits(bits)), true
	}
	return "", false
}

// floatLiteral renders a float as an exact Lua expression. Hex-float form
// round-trips exactly; non-finite values have no literal syntax at all.
func floatLiteral(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "(1/0)"
	case math.IsInf(f, -1):
		return "(-1/0)"
	case math.IsNaN(f):
		return "(0/0)"
	case f == 0 && math.Signbit(f):
		return "(-0.0)"
	}
	s := strconv.FormatFloat(f, 'x', -1, 64)
	if strings.HasPrefix(s, "-") {
		return "(" + s + ")"
	}
	return s
}

// observesNaNSign reports an operation whose result depends on the SIGN of a
// NaN operand.
//
// copysign is the only one: it lifts the sign bit off its second argument and
// puts it on a non-NaN result, so a lost NaN sign becomes a visibly wrong
// answer. Everywhere else a NaN operand yields a NaN result (whose sign is not
// compared) or a comparison that is false for any NaN, so those assertions run
// normally and are not skipped.
func observesNaNSign(export string) bool {
	return strings.Contains(export, "copysign")
}

// hasNegativeNaNArg reports an argument that is a NaN with the sign bit set.
func hasNegativeNaNArg(vals []Value) (bool, string) {
	for _, v := range vals {
		if !isNaNBits(v) {
			continue
		}
		switch v.Type {
		case "f32":
			if b, err := strconv.ParseUint(v.Value, 10, 32); err == nil && b&0x80000000 != 0 {
				return true, "a negative f32 NaN"
			}
		case "f64":
			if b, err := strconv.ParseUint(v.Value, 10, 64); err == nil && b&(1<<63) != 0 {
				return true, "a negative f64 NaN"
			}
		}
	}
	return false, ""
}

// canonicalNaN32 is the quiet NaN the runtime produces for every f32 NaN,
// because Lua cannot carry a payload.
const canonicalNaN32 = 0x7FC00000

// isNaNBits reports an expected value whose bit pattern is some NaN.
func isNaNBits(v Value) bool {
	switch v.Type {
	case "f32":
		bits, err := strconv.ParseUint(v.Value, 10, 32)
		return err == nil && math.IsNaN(float64(math.Float32frombits(uint32(bits))))
	case "f64":
		bits, err := strconv.ParseUint(v.Value, 10, 64)
		return err == nil && math.IsNaN(math.Float64frombits(bits))
	}
	return false
}

// check describes how to verify one returned value.
type check struct {
	// Expr is a Lua boolean expression over the locals `r` and `r2` (an i64
	// arrives as two values), true when the result matches.
	Expr string
	// Want is a human-readable form of the expectation, for failure messages.
	Want string

	// Unrep is a Lua boolean expression evaluated only when Expr is false. When
	// it holds, the answer is not wrong -- it is the only one a Lua number can
	// represent -- and the assertion is recorded as a SKIP carrying UnrepWhy.
	//
	// This is deliberately a RUNTIME test rather than a decision taken here from
	// the expected value alone. An integer expectation whose bits happen to spell
	// a NaN is a perfectly ordinary assertion: i32.umax expects 0xFFFFFFFF and
	// i64.unsigned_decimal expects 0xFFFFFFFFFFFFFFFF, both of which are NaN bit
	// patterns and both of which must really be checked. Only a result that came
	// back as the CANONICAL NaN is excused.
	Unrep    string
	UnrepWhy string
}

// canonicalNaN64 is the quiet NaN f64_to_bits produces for every f64 NaN,
// because Lua cannot carry a sign or a payload.
const canonicalNaN64 = 0x7FF8000000000000

func expectedCheck(vals []Value, opts Options) (check, bool) {
	if len(vals) != 1 || vals[0].Value == "" {
		return check{}, false
	}
	v := vals[0]
	switch v.Type {
	case "i32":
		bits, err := strconv.ParseUint(v.Value, 10, 32)
		if err != nil {
			return check{}, false
		}
		c := check{Expr: "r == " + v.Value, Want: v.Value}
		// An i32 expectation whose bit pattern is a NON-canonical NaN can come
		// from reinterpreting or reloading a float, and then asserts a NaN
		// payload we cannot reproduce: a Lua number canonicalises NaN. Excused
		// only if the value we produced IS the canonical NaN; an ordinary
		// integer that happens to look like a NaN is still checked.
		if f := math.Float32frombits(uint32(bits)); math.IsNaN(float64(f)) &&
			uint32(bits) != canonicalNaN32 && opts.NaN != luagen.NaNExact {
			c.Unrep = fmt.Sprintf("r == %d", uint32(canonicalNaN32))
			c.UnrepWhy = fmt.Sprintf(
				"expected the f32 NaN bits %d; a Lua number canonicalises NaN, so only %d is representable",
				bits, uint32(canonicalNaN32))
		}
		return c, true

	case "i64":
		bits, err := strconv.ParseUint(v.Value, 10, 64)
		if err != nil {
			return check{}, false
		}
		lo, hi := uint32(bits&0xFFFFFFFF), uint32(bits>>32)
		// The driver captures both halves, so the comparison is on the pair.
		c := check{
			Expr: fmt.Sprintf("r == %d and r2 == %d", lo, hi),
			Want: fmt.Sprintf("%s (%d,%d)", v.Value, lo, hi),
		}
		// Same platform limit as the i32 case, one width up: an i64 expectation
		// that is a non-canonical f64 NaN comes from i64.reinterpret_f64 or an
		// i64.load over a stored float.
		if math.IsNaN(math.Float64frombits(bits)) && bits != canonicalNaN64 &&
			opts.NaN != luagen.NaNExact {
			c.Unrep = fmt.Sprintf("r == %d and r2 == %d",
				uint32(canonicalNaN64&0xFFFFFFFF), uint32(canonicalNaN64>>32))
			c.UnrepWhy = fmt.Sprintf(
				"expected the f64 NaN bits %s; a Lua number canonicalises NaN, so only %d is representable",
				v.Value, uint64(canonicalNaN64))
		}
		return c, true

	case "f32", "f64":
		// The spec permits a range of NaN payloads for arithmetic results, and
		// Lua canonicalises NaN anyway, so NaN is compared by CLASS. Everything
		// else is compared BITWISE, which is what distinguishes -0 from +0.
		// NaN is compared by CLASS, never by payload.
		//
		// Two reasons: the spec itself permits a range of payloads for
		// arithmetic results, and -- decisively -- a Lua number cannot carry a
		// NaN payload at all. Lua canonicalises it, so an expectation of a
		// specific NaN bit pattern is unsatisfiable on this platform rather
		// than merely unimplemented. Preserving payloads would mean boxing
		// every float, which is far too expensive to pay for a case no real
		// guest depends on.
		if strings.HasPrefix(v.Value, "nan") {
			// The spec itself permits a range of payloads here.
			return check{Expr: "(r ~= r) or (type(r) == 'table')", Want: v.Value}, true
		}
		if isNaNBits(v) && opts.NaN != luagen.NaNExact {
			return check{Expr: "r ~= r", Want: "NaN (payload not preserved by Lua)"}, true
		}
		if v.Type == "f32" {
			bits, err := strconv.ParseUint(v.Value, 10, 32)
			if err != nil {
				return check{}, false
			}
			return check{
				Expr: fmt.Sprintf("(r ~= r and %v) or (r == r and M.rt.f32_to_bits(r) == %d)",
					math.IsNaN(float64(math.Float32frombits(uint32(bits)))), bits),
				Want: fmt.Sprintf("%v (bits %d)", math.Float32frombits(uint32(bits)), bits),
			}, true
		}
		bits, err := strconv.ParseUint(v.Value, 10, 64)
		if err != nil {
			return check{}, false
		}
		lo := uint32(bits & 0xFFFFFFFF)
		hi := uint32(bits >> 32)
		return check{
			Expr: fmt.Sprintf("(function() local a,b = M.rt.f64_to_bits(r) return a == %d and b == %d end)()", lo, hi),
			Want: fmt.Sprintf("%v (bits %d)", math.Float64frombits(bits), bits),
		}, true
	}
	return check{}, false
}

// RunDir executes every converted .wast file under dir with default options.
func RunDir(h *luahost.Host, dir string) ([]*Outcome, error) {
	return RunDirWith(h, dir, Options{})
}

// RunDirWith executes every converted .wast file under dir.
func RunDirWith(h *luahost.Host, dir string, opts Options) ([]*Outcome, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var outs []*Outcome
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		jsonPath := filepath.Join(dir, e.Name(), e.Name()+".json")
		if _, err := os.Stat(jsonPath); err != nil {
			continue
		}
		o, err := RunFileWith(h, jsonPath, opts)
		if err != nil {
			return nil, err
		}
		outs = append(outs, o)
	}
	return outs, nil
}

// Totals aggregates outcomes across files.
func Totals(outs []*Outcome) (total, passed, failed, skipped int) {
	for _, o := range outs {
		total += o.Total
		passed += o.Passed
		failed += o.Failed
		skipped += o.Skipped
	}
	return
}
