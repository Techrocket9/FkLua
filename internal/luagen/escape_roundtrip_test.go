package luagen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luahost"
)

// Every byte, followed by every byte, round-trips through luaString and the
// real 5.2 parser. 65,536 cases in one chunk.
//
// The escape's width is decided from the NEXT emitted character, so the pair is
// the unit that can fail: a one-digit escape followed by a digit character is
// the whole hazard, and the second byte of each pair also covers "an escape at
// the end of the literal".
func TestEveryBytePairRoundTripsThroughTheLiteral(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	var b strings.Builder
	b.WriteString("local n, bad = 0, 0\n")
	b.WriteString("local function chk(s, b1, b2)\n")
	b.WriteString("  n = n + 1\n")
	b.WriteString("  if #s ~= 2 or s:byte(1) ~= b1 or s:byte(2) ~= b2 then\n")
	b.WriteString("    bad = bad + 1\n")
	b.WriteString("    if bad <= 8 then print(string.format('MISMATCH b1=%d b2=%d len=%d g1=%s g2=%s',\n")
	// `or 'none'`, because s:byte(2) on a one-byte string returns NO values and
	// tostring() with none raises "value expected" before the line can print:
	// the designed MISMATCH message was unreachable for the likeliest wrong
	// answer, a mis-read escape swallowing the byte after it.
	b.WriteString("      b1, b2, #s, tostring(s:byte(1) or 'none'), tostring(s:byte(2) or 'none'))) end\n")
	b.WriteString("  end\nend\n")
	// One function per high byte: constants are per-function in Lua, and a
	// single 65,536-constant chunk is a different thing to be measuring.
	for hi := 0; hi < 256; hi++ {
		b.WriteString(";(function()\n")
		for lo := 0; lo < 256; lo++ {
			fmt.Fprintf(&b, "chk(%s, %d, %d)\n",
				luaString(string([]byte{byte(hi), byte(lo)})), hi, lo)
		}
		b.WriteString("end)()\n")
	}
	b.WriteString("print(string.format('checked %d cases, %d mismatches', n, bad))\n")
	out, err := h.RunString(b.String())
	if err != nil {
		t.Fatalf("lua52f refused the corpus: %v\n%s", err, out)
	}
	if !strings.Contains(out, "checked 65536 cases, 0 mismatches") {
		t.Fatalf("round trip failed:\n%s", out)
	}
	t.Logf("%s", strings.TrimSpace(out))
}
