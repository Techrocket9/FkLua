package factorio

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	luart "github.com/Techrocket9/fklua/runtime"
)

// THE HANDLE SPACE IS SPLIT BY NUMBER, AND FOUR PLACES NOW SAY WHERE.
//
// fk_abi.lua owns the split -- 1..9 globals, 10..0x3FFFFFFF persistent,
// 0x40000000 and up transient -- and it is the only side that can be wrong in a
// way the game sees. Since the guest-side predicates landed (Persistent,
// Transient, Global in Go; is_persistent, is_transient, is_global in Rust) the
// two generated preambles carry the same two numbers so the question can be
// asked without a host call. No compiler checks that, exactly as no compiler
// checks the kind numbers TestKindNumbersMatchTheLuaABI pins, and the failure
// mode is the same shape: nothing raises, a predicate simply answers wrong.
//
// The numbers are READ OUT of fk_abi.lua rather than repeated here, so moving
// the split is one edit and this test follows it. Two properties per language:
// each constant is declared exactly once and equals the ABI's, and each
// predicate's BODY names the constants it compares against instead of spelling
// their digits.
func TestHandleSpaceConstantsMatchTheLuaABI(t *testing.T) {
	abiFirstDynamic := luaLocalNumber(t, luart.ABI(), "FIRST_DYNAMIC")
	abiTransient := luaLocalNumber(t, luart.ABI(), "TRANSIENT")

	// The ABI's own two rows have to be in the order the table says, or the
	// persistent space is empty and every number below is meaningless.
	if abiFirstDynamic < 1 || abiFirstDynamic >= abiTransient {
		t.Fatalf("fk_abi.lua declares FIRST_DYNAMIC=%d and TRANSIENT=%d, which is "+
			"not a space", abiFirstDynamic, abiTransient)
	}

	for _, lang := range []struct {
		name         string
		src          string
		firstDynamic *regexp.Regexp
		transient    *regexp.Regexp
		predicates   []predicateBody
	}{
		{
			name:         "go",
			src:          goRuntime,
			firstDynamic: regexp.MustCompile(`(?m)^\s*FirstDynamicHandle\s+uint32\s*=\s*(\S+)\s*$`),
			transient:    regexp.MustCompile(`(?m)^\s*TransientHandleBase\s+uint32\s*=\s*(\S+)\s*$`),
			predicates: []predicateBody{
				{"Global", regexp.MustCompile(`(?s)func \(o Object\) Global\(\) bool \{(.*?)\}`),
					[]string{"FirstDynamicHandle"}},
				{"Persistent", regexp.MustCompile(`(?s)func \(o Object\) Persistent\(\) bool \{(.*?)\}`),
					[]string{"FirstDynamicHandle", "TransientHandleBase"}},
				{"Transient", regexp.MustCompile(`(?s)func \(o Object\) Transient\(\) bool \{(.*?)\}`),
					[]string{"TransientHandleBase"}},
			},
		},
		{
			name:         "rust",
			src:          rustRuntime,
			firstDynamic: regexp.MustCompile(`(?m)^\s*pub const FIRST_DYNAMIC: u32 = (\S+);\s*$`),
			transient:    regexp.MustCompile(`(?m)^\s*pub const TRANSIENT_BASE: u32 = (\S+);\s*$`),
			predicates: []predicateBody{
				{"is_global", regexp.MustCompile(`(?s)pub fn is_global\(self\) -> bool \{(.*?)\}`),
					[]string{"Self::FIRST_DYNAMIC"}},
				{"is_persistent", regexp.MustCompile(`(?s)pub fn is_persistent\(self\) -> bool \{(.*?)\}`),
					[]string{"Self::FIRST_DYNAMIC", "Self::TRANSIENT_BASE"}},
				{"is_transient", regexp.MustCompile(`(?s)pub fn is_transient\(self\) -> bool \{(.*?)\}`),
					[]string{"Self::TRANSIENT_BASE"}},
			},
		},
	} {
		t.Run(lang.name, func(t *testing.T) {
			for _, c := range []struct {
				name string
				re   *regexp.Regexp
				want uint64
			}{
				{"FIRST_DYNAMIC", lang.firstDynamic, abiFirstDynamic},
				{"TRANSIENT", lang.transient, abiTransient},
			} {
				m := c.re.FindAllStringSubmatch(lang.src, -1)
				// Anti-vacuity in both directions: none means the declaration was
				// renamed and this test would silently stop checking anything;
				// more than one means the ONE PLACE per language it is supposed
				// to live in became two.
				if len(m) != 1 {
					t.Fatalf("%s: expected exactly one declaration matching %s, found %d",
						c.name, c.re, len(m))
				}
				got, err := parseIntLiteral(m[0][1])
				if err != nil {
					t.Fatalf("%s: cannot read %q: %v", c.name, m[0][1], err)
				}
				if got != c.want {
					t.Errorf("%s is %d in the %s preamble and %d in fk_abi.lua -- a "+
						"guest-side predicate that disagrees with the handle table "+
						"answers wrong with nothing raising", c.name, got, lang.name, c.want)
				}
			}

			// ...and every predicate NAMES the constants it compares against
			// rather than inlining their digits, which is the half the value
			// comparison above cannot see: a predicate written with the number in
			// it agrees with fk_abi.lua today and drifts silently the moment the
			// split moves.
			//
			// THE PROPERTY IS ABOUT THE CONSTANT'S DEFINITION AND ITS USES, not
			// about where the digits appear. An earlier form counted integer
			// literals over the WHOLE preamble and required exactly one: it could
			// only ever guard TRANSIENT_BASE -- 10 is far too common a number to
			// count -- so an inlined FIRST_DYNAMIC passed, which is measured in
			// the review this replaced; and it went red on a doc comment that
			// merely MENTIONED 0x40000000, which is a legitimate thing for a doc
			// comment to do. Reading the predicate BODIES covers both constants
			// and cannot be tripped by prose.
			for _, p := range lang.predicates {
				m := p.body.FindAllStringSubmatch(lang.src, -1)
				// Anti-vacuity: a renamed or reshaped predicate must fail rather
				// than quietly stop being checked.
				if len(m) != 1 {
					t.Fatalf("%s: expected exactly one body matching %s, found %d",
						p.name, p.body, len(m))
				}
				body := m[0][1]
				for _, want := range p.names {
					if !strings.Contains(body, want) {
						t.Errorf("%s's body in the %s preamble does not name %s:\n\t%s",
							p.name, lang.name, want, strings.TrimSpace(body))
					}
				}
				for _, split := range []uint64{abiFirstDynamic, abiTransient} {
					if n := countLiteral(body, split); n != 0 {
						t.Errorf("%s's body in the %s preamble spells the split "+
							"point %d as a literal %d time(s); it must name the "+
							"constant, which is the one place the number lives:\n\t%s",
							p.name, lang.name, split, n, strings.TrimSpace(body))
					}
				}
			}
		})
	}
}

// predicateBody is one space predicate: how to find its body, and the constants
// that body has to name.
type predicateBody struct {
	name  string
	body  *regexp.Regexp
	names []string
}

// luaLocalNumber reads `local NAME = <number>` out of a Lua source, ignoring a
// trailing comment. It fails the test rather than returning an error, because
// every caller here would only turn the error into the same failure.
func luaLocalNumber(t *testing.T, src, name string) uint64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^local ` + regexp.QuoteMeta(name) + `\s*=\s*([^\s-]+)`)
	m := re.FindAllStringSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("expected exactly one `local %s = ...` in fk_abi.lua, found %d",
			name, len(m))
	}
	v, err := parseIntLiteral(m[0][1])
	if err != nil {
		t.Fatalf("cannot read fk_abi.lua's %s (%q): %v", name, m[0][1], err)
	}
	return v
}

// parseIntLiteral reads a decimal or 0x literal in any of the three spellings
// the three languages use, underscores included.
func parseIntLiteral(s string) (uint64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), "_", "")
	return strconv.ParseUint(s, 0, 64)
}

var intLiteral = regexp.MustCompile(`\b(?:0[xX][0-9A-Fa-f_]+|[0-9][0-9_]*)\b`)

// countLiteral counts the integer literals in src whose VALUE is want, so a
// decimal and a hex spelling of the same number both count.
func countLiteral(src string, want uint64) int {
	n := 0
	for _, lit := range intLiteral.FindAllString(src, -1) {
		if v, err := parseIntLiteral(lit); err == nil && v == want {
			n++
		}
	}
	return n
}
