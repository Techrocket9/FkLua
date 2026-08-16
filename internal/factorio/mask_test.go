package factorio

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
)

// A MASKED FIELD READS AS EMPTY, AND THE STALE BUFFER IS WHY IT IS WRITTEN AT
// ALL.
//
// The obvious implementation of a field mask is "do not write the field", and
// it is wrong here for a reason that has nothing to do with the mask: the event
// scratch buffer is allocated once and REUSED for every dispatch at that
// nesting level. A field left alone therefore shows the guest whatever the
// previous event put at those bytes -- a presence byte still reading 1 over a
// pointer that has since been reclaimed, which is the silent-garbage class this
// ABI is built to refuse.
//
// So write_struct zeroes the header instead: two stores against a deep copy.
// This test writes the block TWICE into the same address, with data the first
// time and a mask the second, which is exactly the dispatch sequence and the
// only way to see the difference.
func TestAMaskedFieldReadsAsEmptyNotStale(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "player_index", Kind: KindU32},
		{Name: "actions", Kind: KindArray, Elem: &FieldSpec{Kind: KindU32}},
		{Name: "note", Kind: KindString, Optional: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 8 return p end, function() end)

-- Dispatch one: the whole payload, into the buffer at 1024.
H.write_struct(fields, 1024, { player_index = 7, actions = {1,2,3}, note = "hi" })
local a = H.read_struct(fields, 1024)
print("full " .. a.player_index .. " " .. #a.actions .. " " .. tostring(a.note))

-- Dispatch two: the SAME buffer, with actions (index 2) and note (index 3)
-- masked. Nothing else about the call changes.
local masked = H.mask_fields(fields, 2 + 4)
H.write_struct(masked, 1024, { player_index = 9, actions = {4,5,6,7}, note = "bye" })
local c = H.read_struct(masked, 1024)
print("masked " .. c.player_index .. " " .. #c.actions .. " " .. tostring(c.note))

-- And the guest reads the block at its COMPILED-IN offsets, which the mask
-- must not have moved. Read it back through the UNMASKED description, which
-- is what a generated decoder is.
local d = H.read_struct(fields, 1024)
print("asguest " .. d.player_index .. " " .. #d.actions .. " " .. tostring(d.note))
`, b.LuaTable()))

	want := strings.Join([]string{
		"full 7 3 hi",
		"masked 9 0 nil",
		"asguest 9 0 nil",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A MANDATORY SCALAR IS NOT MASKABLE, and asking is logged rather than obeyed.
//
// Masking one would write nothing and leave the guest reading a stale word, or
// zero it and leave the guest reading a zero it cannot tell from a real value.
// Both are the silent-wrong-value failure; encoding it anyway costs time and
// nothing else, which is the same widening direction an unreadable filter
// takes. The refusal names the field so the author can see which bit was
// ignored.
func TestAMandatoryScalarIsRefusedByTheFieldMask(t *testing.T) {
	b, err := LayoutStruct([]FieldSpec{
		{Name: "player_index", Kind: KindU32},
		{Name: "actions", Kind: KindArray, Elem: &FieldSpec{Kind: KindU32}},
		{Name: "tick", Kind: KindU32},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runMarshal(t, fmt.Sprintf(`
local fields = %s
local next_ = 8192
H.bind_alloc(function(n) local p = next_ next_ = next_ + n + 8 return p end, function() end)
-- Ask for all three: two mandatory scalars and one container.
local masked, refused = H.mask_fields(fields, 1 + 2 + 4)
print("refused " .. table.concat(refused, ","))
H.write_struct(masked, 1024, { player_index = 7, actions = {1,2,3}, tick = 99 })
local r = H.read_struct(fields, 1024)
print("kept " .. r.player_index .. " " .. r.tick .. " actions=" .. #r.actions)
`, b.LuaTable()))

	want := "refused player_index,tick\nkept 7 99 actions=0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// maskGuest packages a guest whose fk_on_init subscribes to on_undo_applied
// with the given field mask, and whose fk_on_event does nothing.
//
// The dispatch protocol is REAL: this writes a package, and the stub below
// requires the packaged control.lua and calls the handler Factorio would call.
// (The round-3 note pointed at internal/luagen's twoInstancesWith for this; that
// harness takes a PersistMode and replays the persistence protocol through a
// stand-in storage -- it never loads fk_mod.lua and never dispatches an event.
// This one, the filter tests' harness, is the one that replays dispatch.)
func maskGuest(t *testing.T, name string, mask int) string {
	t.Helper()
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	events := GenerateEvents(a)

	var undoID int
	for _, e := range events.Events {
		if e.Name == "on_undo_applied" {
			undoID = e.ID
		}
	}
	if undoID == 0 {
		t.Skip("this API has no on_undo_applied")
	}

	wat := fmt.Sprintf(`(module
		(import "fk" "subscribe" (func $sub (param i32 i32 i32) (result i32)))
		(memory 16)
		;; Two allocators, because the timing leg below runs thousands of
		;; marshalling allocations through a guest that has no arena. fk_alloc
		;; RINGS -- nothing here reads a marshalled value back, and the host is
		;; finished with the block before the call returns -- while
		;; fk_alloc_static must not be recycled at all: it is where the event
		;; scratch buffer lives, and that outlives every call.
		(global $heap (mut i32) (i32.const 65536))
		(global $static (mut i32) (i32.const 4096))
		(func (export "fk_alloc") (param $n i32) (result i32) (local $p i32)
			(if (i32.gt_u (i32.add (global.get $heap) (local.get $n))
			              (i32.const 900000))
				(then (global.set $heap (i32.const 65536))))
			(local.set $p (global.get $heap))
			(global.set $heap (i32.add (global.get $heap) (local.get $n)))
			(local.get $p))
		(func (export "fk_alloc_static") (param $n i32) (result i32) (local $p i32)
			(local.set $p (global.get $static))
			(global.set $static (i32.add (global.get $static) (local.get $n)))
			(local.get $p))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_init")
			(drop (call $sub (i32.const %d) (i32.const 0) (i32.const %d))))
		(func (export "fk_on_event") (param $id i32) (param $ptr i32)))`,
		undoID, mask)

	im := buildIR(t, wat)
	used, _ := UsedMembers(im)
	usedEv, _ := UsedEvents(im)
	apiSrc, err := full.Only(used).LuaSourceWith(a, events.Only(usedEv))
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: name, Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk,
		Exports: []string{"fk_on_init", "fk_on_event", "fk_alloc",
			"fk_alloc_static", "fk_free"},
		APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// maskStub raises on_undo_applied N times with an `actions` value that COUNTS
// every access, so "was the deep copy made" is a number rather than a timing.
const maskStub = `
package.path = %q
local logged = {}
function log(s) logged[#logged+1] = s end
defines = { events = { on_tick = 1, on_undo_applied = 2 } }
storage = {}
local handlers = {}
script = {
  mod_name = "t",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f, flt) handlers[ev] = f end,
  set_event_filter = function(ev, flt) end,
}

require("control")
handlers.on_init()

local touched = 0
local actions = setmetatable({}, {
  __len = function() touched = touched + 1 return 3 end,
  __index = function() touched = touched + 1 return { type = "built-entity" } end,
})
handlers[2]({ player_index = 1, actions = actions, name = 1, tick = 0 })
print("touched " .. touched)
for i = 1, #logged do
  if logged[i]:find("skip") then print("log " .. logged[i]) end
end
`

// A SUBSCRIPTION CARRIES ITS FIELD MASK, and a masked container is never walked.
//
// on_undo_applied is the shape the first downstream mod reported: its `actions`
// is an array of tier-2 values, so the eager encode deep-copies every
// BlueprintEntity in an undo step before a handler that wants one uint32 is
// entered. `actions` is field index 2, hence bit 1.
//
// The assertion is on ACCESS COUNT rather than on time: write_array asks for #v
// and then indexes it, so a proxy table says exactly whether the copy happened.
// A timing would say the same thing less reliably and only on a quiet machine.
func TestASubscriptionCarriesItsFieldMask(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	for _, tc := range []struct {
		name string
		mask int
		want string
	}{
		{"unmasked", 0, "touched 4"},
		{"masked", 2, "touched 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := maskGuest(t, "fk-mask-"+tc.name, tc.mask)
			out, err := h.RunString(fmt.Sprintf(maskStub, filepath.Join(dir, "?.lua")))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := strings.TrimSpace(out); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A mask over a mandatory scalar is refused AT SUBSCRIBE TIME, logged, and the
// mod goes on running -- the same widening direction an unreadable filter takes.
// player_index is field index 1, hence bit 0.
func TestAMaskOverAMandatoryFieldIsLoggedAndIgnored(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := maskGuest(t, "fk-mask-bad", 1)
	out, err := h.RunString(fmt.Sprintf(maskStub, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.TrimSpace(out)
	if !strings.Contains(got, "touched 4") {
		t.Errorf("the refused mask changed the encode: %q", got)
	}
	if !strings.Contains(got, "player_index") {
		t.Errorf("the refusal did not name the field:\n%s", got)
	}
}

// costStub raises on_undo_applied N times with a REAL actions array of the
// given size, each action a tier-2 table with a nested entity list -- which is
// what makes the eager encode a deep copy rather than a memcpy.
const costStub = `
package.path = %q
function log(s) end
defines = { events = { on_tick = 1, on_undo_applied = 2 } }
storage = {}
local handlers = {}
script = {
  mod_name = "t",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f, flt) handlers[ev] = f end,
  set_event_filter = function(ev, flt) end,
}
require("control")
handlers.on_init()

local actions = {}
for i = 1, %d do
  actions[i] = { type = "built-entity", target = {
    { name = "transport-belt", position = { x = i, y = 2 }, direction = 4 },
    { name = "underground-belt", position = { x = i, y = 3 }, direction = 2 },
  } }
end
local e = { player_index = 1, actions = actions, name = 1, tick = 0 }
for i = 1, %d do handlers[2](e) end
print("done")
`

// WHAT THE MASK IS WORTH, measured on the shape that motivated it.
//
// on_undo_applied's `actions` is an array of tier-2 values, so the eager encode
// runs write_dyn over every entry -- a full deep copy of an undo step's
// BlueprintEntity list -- before the guest that wants one uint32 is entered.
//
// Timing comes from OUTSIDE the process, because os.clock is nil: lua52f is
// patched to Factorio's sandbox and Factorio removes the os library. So the
// same script runs at N dispatches and at zero, and the two are differenced --
// the pattern the ABI cost tests already use, and for the same reason.
func TestWhatAnEventFieldMaskIsWorth(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	const iters = 100

	run := func(dir string, actions, n int) time.Duration {
		script := fmt.Sprintf(costStub, filepath.Join(dir, "?.lua"), actions, n)
		best := time.Duration(1<<62 - 1)
		for try := 0; try < 2; try++ {
			t0 := time.Now()
			if _, err := h.RunString(script); err != nil {
				t.Fatalf("run: %v", err)
			}
			if d := time.Since(t0); d < best {
				best = d
			}
		}
		return best
	}

	fullDir := maskGuest(t, "fk-cost-full", 0)
	maskDir := maskGuest(t, "fk-cost-masked", 2)
	per := func(dir string, actions int) time.Duration {
		return (run(dir, actions, iters) - run(dir, actions, 0)) / iters
	}

	// TWO SIZES, because one would be quotable as a headline and this cost is
	// linear in the array: the mask removes the whole of write_dyn for that
	// field, so what it is worth is exactly what the field cost, and that is a
	// property of the payload rather than of the mask.
	var full, masked time.Duration
	for _, actions := range []int{20, 200} {
		full, masked = per(fullDir, actions), per(maskDir, actions)
		t.Logf("on_undo_applied, %3d actions x 2 entities: unmasked %10v/dispatch, "+
			"masked %8v/dispatch", actions, full, masked)
	}

	// A variant that does strictly less work cannot cost more. The threshold is
	// deliberately loose -- this asserts the mechanism is engaged, and the ratio
	// above is the number to read.
	if masked >= full {
		t.Errorf("masking the deep-copied field did not reduce the encode: "+
			"unmasked %v, masked %v", full, masked)
	}
}
