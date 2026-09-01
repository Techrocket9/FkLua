package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luahost"
	luart "github.com/Techrocket9/fklua/runtime"
)

// WHAT A SECOND RELEASE ACTUALLY DOES, AGAINST THE VERBATIM HANDLE TABLE.
//
// The Rust binding's Object::release used to say that releasing one slot twice
// is harmless because "the host checks the slot before freeing it", and
// docs/factorio-api.md said the same in public. It is false, and the direction
// it is false in is the dangerous one: it is the sentence that tells an author
// the shape below is safe.
//
// fk_abi.lua checks that the slot is OCCUPIED, not by whom (M.release: `if
// persistent[h] == nil then return ERR_BAD_HANDLE end`). The free list is a LIFO
// stack during play (M.release pushes, M.retain pops the top), so the slot a
// release just freed is the VERY NEXT one handed out. That makes the bad case
// the default rather than the rare one, and it is ABA rather than a double free:
// by the time the second release lands, the slot belongs to somebody else, the
// call answers OK, and the original owner goes on naming a slot a fourth owner
// now holds. Nothing reports any of it.
//
// THIS IS WHY Object::retained REFUSES A PERSISTENT HANDLE. A guard is born at
// the promotion and nowhere else, so two guards cannot name one slot; the
// sequence below is what a caller reaches only by taking the raw retain/release
// escape hatch and getting it wrong. Keeping it as a test keeps the doc honest:
// if a future handle table ever made a stale release detectable, this goes red
// and both bindings' wording has to move with it.
func TestReleasingOneSlotTwiceFreesAnotherOwnersObject(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ABIFile), []byte(luart.ABI()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunString(fmt.Sprintf(`package.path = %q
local M = require(%q)

-- Three stand-in LuaObjects. What a LuaObject looks like from this side is a
-- table with `+"`valid`"+` and something to read back, which is all the handle table
-- ever touches.
local A = { valid = true, name = "A" }
local C = { valid = true, name = "C" }
local D = { valid = true, name = "D" }

-- Owner A promotes a TRANSIENT handle, which mints a fresh slot.
local pa = M.retain(M.transient(A))
print("A owns slot " .. pa)

-- The second owner, and the whole defect in one line: retain is IDEMPOTENT for
-- a handle already persistent, so it hands the same number back and allocates
-- nothing. This is what Retained::new(*guard) did through Deref.
local pb = M.retain(pa)
print("B owns slot " .. pb .. " same=" .. tostring(pb == pa))

-- A drops.
print("A releases, status " .. M.release(pa))

-- C promotes a DIFFERENT object, and the free list is LIFO, so C gets A's slot.
local pc = M.retain(M.transient(C))
print("C owns slot " .. pc)

-- B drops. The slot is occupied, so this is not ERR_BAD_HANDLE: it frees the
-- slot C owns and answers OK.
print("B releases, status " .. M.release(pb))

-- C is still holding its number, and the number now names nothing.
local _, st = M.get(pc)
print("C reads its slot, status " .. st)

-- ...and the slot goes to a fourth owner while C still names it.
print("D owns slot " .. M.retain(M.transient(D)))
local obj = M.get(pc)
print("C reads its slot and gets " .. tostring(obj and obj.name))
`, filepath.Join(dir, "?.lua"), strings.TrimSuffix(ABIFile, ".lua")))
	if err != nil {
		t.Fatalf("driving the handle table: %v\n%s", err, out)
	}

	want := strings.Join([]string{
		"A owns slot 10",           // the first slot the persistent space has
		"B owns slot 10 same=true", // no allocation: one slot, two owners
		"A releases, status 0",
		"C owns slot 10",       // LIFO, so the freed slot is the next one out
		"B releases, status 0", // NOT ERR_BAD_HANDLE: it freed C's slot
		"C reads its slot, status 1",
		"D owns slot 10",
		"C reads its slot and gets D", // two owners, one slot, no status anywhere
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a second release of one slot is not "+
			"harmless: the host checks that the slot is occupied, not by whom, "+
			"and the free list is LIFO. If this is what changed, the release doc "+
			"in both preambles and in docs/factorio-api.md changes with it)",
			got, want)
	}
}
