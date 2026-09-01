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

// WHAT A TRANSIENT HANDLE KEPT PAST ITS DISPATCH ACTUALLY NAMES.
//
// Both generated preambles used to say that a number "left over from an earlier
// dispatch names nothing and answers BAD_HANDLE on the next call", and
// docs/factorio-api.md carried the same implication. The first half of that
// sentence -- a number the host never handed out -- is true. The second half is
// the opposite of what the handle table does, and it is false in the direction
// that hands a guard somebody else's object.
//
// M.clear_transient RESETS THE COUNTER (`transient = {}; nextT = TRANSIENT`), so
// 0x40000000 is handed out again on the very next OUTERMOST dispatch, to
// whatever that dispatch asked for first. A stale number therefore resolves with OK to a
// DIFFERENT object, and a retain of it succeeds and mints a real slot over that
// object. Nothing reports any of it: the three space predicates are compares on
// the number, and the number is in range.
//
// It answers BAD_HANDLE only in the other case, which the second half below
// pins: a dispatch that has not allocated that far leaves the index empty. Both
// halves are the doc's sentence, so both are here -- a test that only showed the
// reuse would be green on a handle table that had stopped reusing but also
// stopped resolving.
//
// THIS IS WHY THE RULE IS THE CALLER'S AND NOT A PREDICATE'S. Object::retained
// cannot refuse this shape: the retain succeeds. Retain inside the dispatch that
// produced the handle. fk_mod.lua says what the cost is when nobody does
// ("clear_transient restarts the id counter, so the outer handle could come back
// pointing at a different object, which in a lockstep game is a desync rather
// than an error") and calls dispatch_done, which is where H.clear_transient is
// reached, only when the OUTERMOST dispatch returns.
//
// THE NESTING HALF IS NOT PINNED HERE AND CANNOT BE. This harness drives
// fk_abi.lua alone and calls M.clear_transient by hand, so it stands in for a
// dispatch boundary rather than being one, and `depth` lives one file over.
// TestANestedDispatchLeavesTheOuterOneIntact in reentrant_test.go is the leg
// that runs a real re-entrant dispatch through fk_mod.lua and asserts that the
// inner one neither released the outer handle nor restarted the counter.
//
// If this goes red, the transient predicate's doc in both preambles,
// Object::retained's own paragraph, docs/factorio-api.md and docs/from-lua.md
// all move with it: they state this behaviour as the reason for the rule.
func TestAStaleTransientHandleNamesSomebodyElsesObject(t *testing.T) {
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

-- Two stand-in LuaObjects, one per dispatch. A dispatch is M.transient calls
-- followed by the M.clear_transient that fk_mod.lua's dispatch_done makes when
-- the outermost one returns.
local A = { valid = true, name = "A" }
local B = { valid = true, name = "B" }

-- DISPATCH 1. The guest is handed a number and (the defect) keeps it.
local kept = M.transient(A)
print("dispatch 1: A is " .. kept)
M.clear_transient()

-- DISPATCH 2, which asks the host for one object of its own.
local fresh = M.transient(B)
print("dispatch 2: B is " .. fresh .. ", same number: " .. tostring(fresh == kept))

-- The number the guest kept is live again, and it is not A.
local obj, st = M.get(kept)
print("A's old handle reads " .. tostring(obj and obj.name) .. ", status " .. st)

-- ...and promoting it succeeds, so a guard born here owns a real slot over an
-- object its holder never named.
local slot, rst = M.retain(kept)
local held = M.get(slot)
print("retaining A's old handle: slot " .. slot .. ", holding " ..
  tostring(held and held.name) .. ", status " .. rst)
M.release(slot)
M.clear_transient()

-- THE OTHER HALF. A dispatch that hands out FEWER handles than the stale number
-- leaves that index empty, which is the only case the old sentence described.
local first = M.transient(A)
local second = M.transient(B)
print("dispatch 3: the second handle is " .. second)
M.clear_transient()
local gone, gst = M.get(second)
print("dispatch 4 asks for one handle: " .. M.transient(A))
print("the second handle now reads " .. tostring(gone and gone.name) .. ", status " .. gst)
local nslot, nst = M.retain(second)
print("retaining it: slot " .. nslot .. ", status " .. nst)
local _ = first
`, filepath.Join(dir, "?.lua"), strings.TrimSuffix(ABIFile, ".lua")))
	if err != nil {
		t.Fatalf("driving the handle table: %v\n%s", err, out)
	}

	want := strings.Join([]string{
		// The first transient number of every dispatch is the same number.
		"dispatch 1: A is 1073741824",
		"dispatch 2: B is 1073741824, same number: true",
		// NOT nil and NOT ERR_BAD_HANDLE: the kept number names B.
		"A's old handle reads B, status 0",
		// ...and the promotion the guard is born from succeeds over B.
		"retaining A's old handle: slot 10, holding B, status 0",
		"dispatch 3: the second handle is 1073741825",
		// Only here is the old doc's sentence true: nothing was put at that
		// index, so the number names nothing.
		"dispatch 4 asks for one handle: 1073741824",
		"the second handle now reads nil, status 1",
		"retaining it: slot 0, status 1",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a transient number is REUSED: "+
			"M.clear_transient resets the counter, so a handle kept past its "+
			"dispatch names whatever the next dispatch put at that index, with "+
			"OK. If this is what changed, the transient predicate's doc in both "+
			"preambles, Object::retained's paragraph, docs/factorio-api.md and "+
			"docs/from-lua.md change with it)", got, want)
	}
}

// A GLOBAL HANDLE IS A NUMBER THAT IS NEVER REUSED, NOT A HANDLE THAT IS LIVE.
//
// The same defect one predicate over. is_global's doc claimed the globals range
// was "the one range where membership also says the handle is live, since 1..9
// are never allocated and never freed". The second clause is true and the
// conclusion does not follow: fk_abi.lua resolves a global through genv[name] on
// EVERY access, deliberately, because `game` does not exist while control.lua is
// loading nor inside Factorio's own on_load, and binding the nine once at load
// would bind nine nils for the life of the session (the genv comment says so).
//
// So there are two windows in which a global answers ERR_BAD_HANDLE and its
// retain comes back 0, and the first of them is reachable by ordinary guest
// code: a guest's package initialisers run while control.lua is loading, which
// is where arm_deferred's own comment puts them. What the range really buys is
// that the NUMBER is never handed out for something else, which is the one thing
// neither dynamic space gives -- a persistent slot is freed and reused, and the
// transient counter restarts when the outermost dispatch returns.
//
// If this goes red, is_global's doc in both preambles and the globals paragraph
// in docs/factorio-api.md move with it.
func TestAGlobalIsNotLiveUntilTheGameBindsIt(t *testing.T) {
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

local function probe(what)
  local obj, st = M.get(2)
  local h, hst = M.retain(2)
  print(what .. ": get " .. tostring(obj ~= nil) .. " status " .. st ..
    ", retain " .. h .. " status " .. hst)
end

print("handle 2 is " .. M.GLOBAL_NAMES[2])

-- Nothing has said where the globals live yet.
probe("before bind_globals")

-- Bound, to an environment in which the game global does not exist: control.lua
-- still loading, or inside Factorio's own on_load.
M.bind_globals({})
probe("bound before the game exists")

-- And once the game is there.
M.bind_globals({ game = { valid = true } })
probe("bound to a live game")
print("releasing it: " .. M.release(2))
`, filepath.Join(dir, "?.lua"), strings.TrimSuffix(ABIFile, ".lua")))
	if err != nil {
		t.Fatalf("driving the handle table: %v\n%s", err, out)
	}

	want := strings.Join([]string{
		"handle 2 is game",
		// In range, and dead. is_global answers true for all three lines.
		"before bind_globals: get false status 1, retain 0 status 1",
		"bound before the game exists: get false status 1, retain 0 status 1",
		// Only here does the number resolve, and only here is retain the no-op
		// that hands the same number back.
		"bound to a live game: get true status 0, retain 2 status 0",
		// ...and it is still not a slot this guest owns.
		"releasing it: 1",
	}, "\n")
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a global is resolved by NAME out of the "+
			"game's environment on every access, so membership of 1..9 says the "+
			"number is never reused, not that the handle is live. If this is what "+
			"changed, is_global's doc in both preambles and the globals paragraph "+
			"in docs/factorio-api.md change with it)", got, want)
	}
}
