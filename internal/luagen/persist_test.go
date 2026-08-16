package luagen

import (
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// twoInstances runs `script` with a factory `mk` in scope, so a test can build
// two INDEPENDENT instances of the same module and carry state from one to the
// other. That is the shape of a save/load: the second instance is a fresh
// process's worth of state, and nothing but what went through `persist`
// connects them.
func twoInstances(t *testing.T, wat, script string, lvl analysis.Level) string {
	t.Helper()
	return twoInstancesWith(t, wat, script, lvl, PersistNone)
}

// twoInstancesWith is twoInstances in a chosen persistence mode. The modes do
// not share a save path -- table hands over the live word table, packed hands
// over string pages and rebuilds -- so one of them being right says nothing
// about the other.
func twoInstancesWith(t *testing.T, wat, script string, lvl analysis.Level, mode PersistMode) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: lvl, Persist: mode})
	if err != nil {
		t.Fatalf("emit at -opt=%s: %v", lvl, err)
	}
	var b strings.Builder
	b.WriteString("local mk = function(...)\n")
	b.WriteString(src)
	b.WriteString("\nend\n")
	b.WriteString(script)
	out, err := h.RunString(b.String())
	if err != nil {
		t.Fatalf("run at -opt=%s: %v", lvl, err)
	}
	return strings.TrimSpace(out)
}

const statefulWAT = `(module
	(memory 1)
	(global $calls (mut i32) (i32.const 0))
	(global $wide (mut i64) (i64.const 0))
	(global $konst i32 (i32.const 99))
	(func (export "bump") (param $at i32) (result i32)
		(global.set $calls (i32.add (global.get $calls) (i32.const 1)))
		(global.set $wide (i64.add (global.get $wide) (i64.const 4294967296)))
		(i32.store (local.get $at) (i32.add (i32.load (local.get $at)) (i32.const 10)))
		(i32.load (local.get $at)))
	(func (export "peek") (param $at i32) (result i32) (i32.load (local.get $at)))
	(func (export "calls") (result i32) (global.get $calls))
	(func (export "widehi") (result i32)
		(i32.wrap_i64 (i64.shr_u (global.get $wide) (i64.const 32)))))`

// The whole mechanism in one assertion: a SECOND instance, built from scratch,
// sees the first one's memory and globals after adopting them.
//
// What makes this work at all is that Lua gives one upvalue cell per local per
// enclosing scope, shared by every closure made in it. So `adopt` assigning MEM
// is seen by F[0], which captured the same cell. If it were not shared, the
// host could read guest state and never install any, and this test would report
// the fresh instance's zeroes.
func TestASecondInstanceAdoptsTheFirstsState(t *testing.T) {
	const script = `
local a = mk({})
a.exports["bump"](0)                    -- memory 0 -> 10, calls 1
a.exports["bump"](0)                    -- memory 0 -> 20, calls 2
a.exports["bump"](64)                   -- memory 64 -> 10, calls 3
local mem, size = a.persist.memory()
local globals = a.persist.globals()

local b = mk({})
print("fresh mem  " .. b.exports["peek"](0))
print("fresh call " .. b.exports["calls"]())
b.persist.adopt(mem, size)
b.persist.setglobals(globals)
print("adopted 0  " .. b.exports["peek"](0))
print("adopted 64 " .. b.exports["peek"](64))
print("calls      " .. b.exports["calls"]())
print("wide hi    " .. b.exports["widehi"]())
print("continues  " .. b.exports["bump"](0))
`
	want := strings.Join([]string{
		"fresh mem  0",
		"fresh call 0",
		"adopted 0  20",
		"adopted 64 10",
		"calls      3",
		"wide hi    3", // an i64 global survives as a (lo, hi) pair
		"continues  30",
	}, "\n")

	for _, lvl := range allLevels {
		if got := twoInstances(t, statefulWAT, script, lvl); got != want {
			t.Errorf("-opt=%s:\ngot:\n%s\nwant:\n%s", lvl, got, want)
		}
	}
}

// An immutable global is not persisted, and must not be: it cannot have changed,
// so a reload rebuilds it from its own initialiser. Persisting it would create
// state a migration has to reconcile for nothing.
func TestAnImmutableGlobalIsNotPersisted(t *testing.T) {
	src := emitBody(t, statefulWAT, analysis.O2)
	i := strings.Index(src, "persist = {")
	if i < 0 {
		t.Fatalf("no persist surface:\n%s", src)
	}
	block := src[i:]
	if j := strings.Index(block, "rt = {"); j > 0 {
		block = block[:j]
	}
	// g0 is $calls, g1/g1h is $wide, g2 is $konst.
	if !strings.Contains(block, "g0") || !strings.Contains(block, "g1h") {
		t.Errorf("both mutable globals belong here:\n%s", block)
	}
	if strings.Contains(block, "g2") {
		t.Errorf("the immutable global does not:\n%s", block)
	}
}

// A module with nothing to carry gets no surface at all, rather than an empty
// one that reads as though persistence is available.
func TestAStatelessModuleHasNoPersistSurface(t *testing.T) {
	src := emitBody(t, `(module (func (export "f") (result i32) (i32.const 1)))`,
		analysis.O2)
	if strings.Contains(src, "persist") {
		t.Errorf("nothing here can outlive a call:\n%s", src)
	}
}

// Memory that GREW has to come back the same size, or every bounds check after
// the load is computed against the wrong limit -- which fails open, reading a
// nil word rather than trapping.
func TestGrownMemorySurvivesAtItsGrownSize(t *testing.T) {
	const wat = `(module
		(memory 1 4)
		(func (export "grow") (result i32) (memory.grow (i32.const 2)))
		(func (export "size") (result i32) (memory.size))
		(func (export "poke") (param $at i32) (i32.store (local.get $at) (i32.const 7)))
		(func (export "peek") (param $at i32) (result i32) (i32.load (local.get $at))))`
	const script = `
local a = mk({})
a.exports["grow"]()
a.exports["poke"](131072)               -- inside page 2, which only exists after the grow
local mem, size = a.persist.memory()

local b = mk({})
b.persist.adopt(mem, size)
print("size " .. b.exports["size"]())
print("word " .. b.exports["peek"](131072))
`
	want := "size 3\nword 7"
	for _, lvl := range allLevels {
		if got := twoInstances(t, wat, script, lvl); got != want {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, want)
		}
	}
}

// The packed twin, and it has to replay control.lua's protocol rather than
// carry the live values across.
//
// The table-mode test above hands `size` straight from one instance to the
// other, which is precisely the step control.lua does NOT do: what a load
// actually reads is the mirror in `storage`, and in packed mode nothing
// refreshed that mirror after a grow. A test that carries the live value
// cannot see the difference -- which is how this survived the milestone whose
// headline was the same bug in table mode.
//
// Three things have to hold, and each was broken on its own:
//   - the size mirror is refreshed, so the guest comes back at its grown size;
//   - restore walks the pages that size implies rather than the array it was
//     given, which a sparse flush leaves holes in;
//   - a page the guest grew into but never wrote reads as ZERO rather than nil.
func TestAGrownPackedHeapSurvivesTheStorageMirror(t *testing.T) {
	const wat = `(module
		(memory 1 4)
		(func (export "grow") (result i32) (memory.grow (i32.const 2)))
		(func (export "size") (result i32) (memory.size))
		(func (export "poke") (param $at i32) (i32.store (local.get $at) (i32.const 7)))
		(func (export "peek") (param $at i32) (result i32) (i32.load (local.get $at))))`
	const script = `
-- Stands in for Factorio's storage. Every value the load reads comes through
-- here, because that is the only thing a save actually carries.
local storage = {}

local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()            -- state_init
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["grow"]()                            -- one page becomes three
a.exports["poke"](131072)                      -- page 2, which the grow created

a.persist.flush(storage.fk_pages)              -- sync_memory
local _, live = a.persist.memory()
if storage.fk_memsize ~= live then storage.fk_memsize = live end

local b = mk({})                               -- the next load
b.persist.restore(storage.fk_pages, storage.fk_memsize)   -- state_load
print("size " .. b.exports["size"]())
print("word " .. tostring(b.exports["peek"](131072)))
print("gap " .. tostring(b.exports["peek"](70000)))
`
	want := "size 3\nword 7\ngap 0"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, want)
		}
	}
}

// THE GROW THAT CROSSES THE 4 MiB WALL, in both persisting modes.
//
// The two tests above grow one page into three, which is the shape both grow
// bugs hid behind and is right for what they pin. It is also three orders of
// magnitude below the size where a grow starts to MATTER: 4 MiB of linear
// memory is 2^20 words, and in Factorio's Lua that is where the word table
// stops behaving like an array -- a 2.7-second tick to cross it and ~20x on
// every access afterwards, measured in game and written up in agents/gc.md,
// "The 4 MiB wall".
//
// NOTHING HERE MEASURES THAT, AND THAT IS THE POINT WORTH STATING. bin/lua52f
// is stock 5.2.1, whose array part grows to 2^30, so it prices the same
// crossing at 3.0 ms against 1.3 -- a 2.3x slope where the game has a 27x
// cliff. A timing assertion written here would pass on a machine where the game
// falls over. What a host-side test CAN hold is that the crossing grow is still
// CORRECT, which is the half a later change to mem_grow's shape -- chunking it,
// reordering it, filling a fresh table and swapping -- would be most likely to
// break, and which no in-game run checks byte for byte.
//
// 60 pages grown by 8 puts the top of the old memory below 2^20 words and the
// top of the new one above it, so the assertions straddle the boundary rather
// than sitting near it.
func TestAGrowAcrossTheFourMiBWallIsStillCorrect(t *testing.T) {
	const wat = `(module
		(memory 60 80)
		(func (export "grow") (result i32) (memory.grow (i32.const 8)))
		(func (export "size") (result i32) (memory.size))
		(func (export "poke") (param $at i32) (i32.store (local.get $at) (i32.const 7)))
		(func (export "peek") (param $at i32) (result i32) (i32.load (local.get $at))))`

	// 60 pages is 3,932,160 bytes, so the old top word starts at 3,932,156 and
	// the grown memory reaches 4,456,448 bytes. 4 MiB is byte 4,194,304, which is
	// the first word of the new region on the far side of the wall.
	const script = `
local a = mk({})
a.exports["poke"](3932156)              -- last word of the ORIGINAL memory
a.exports["grow"]()
a.exports["poke"](4194304)              -- first word PAST 4 MiB, reachable only after the grow
local mem, size = a.persist.memory()

local b = mk({})
b.persist.adopt(mem, size)
print("size " .. b.exports["size"]())
print("old " .. b.exports["peek"](3932156))
print("new " .. b.exports["peek"](4194304))
print("zero " .. b.exports["peek"](4000000))   -- grown into, never written
`
	want := "size 68\nold 7\nnew 7\nzero 0"
	if got := twoInstances(t, wat, script, analysis.O3); got != want {
		t.Errorf("table mode: got %q, want %q", got, want)
	}

	// The packed twin goes through the storage mirror, for the same reason
	// TestAGrownPackedHeapSurvivesTheStorageMirror does: what a load reads is the
	// mirror, and a grow is exactly what used to leave it stale.
	const packedScript = `
local storage = {}
local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["poke"](3932156)
a.exports["grow"]()
a.exports["poke"](4194304)

a.persist.flush(storage.fk_pages)
local _, live = a.persist.memory()
if storage.fk_memsize ~= live then storage.fk_memsize = live end

local b = mk({})
b.persist.restore(storage.fk_pages, storage.fk_memsize)
print("size " .. b.exports["size"]())
print("old " .. tostring(b.exports["peek"](3932156)))
print("new " .. tostring(b.exports["peek"](4194304)))
print("zero " .. tostring(b.exports["peek"](4000000)))
`
	if got := twoInstancesWith(t, wat, packedScript, analysis.O3, PersistPacked); got != want {
		t.Errorf("packed mode: got %q, want %q", got, want)
	}
}

// A HOST write into guest memory has to make the page dirty, exactly as a guest
// store does.
//
// fk_wstr is how the ABI marshals a string back to the guest -- every host call
// returning one goes through it -- and it wrote the head and tail through
// st8raw and the aligned body straight into the word table, marking nothing at
// all. In packed mode the bytes were live in the word table and absent from
// every page flush after them, so they vanished one save/load cycle later,
// nowhere near the call that wrote them.
//
// The string crosses a 4 KiB packed-page boundary on purpose: marking only the
// address it starts at would flush the first page and still lose the rest. The
// guest store is the control -- it lands in a page nothing else touches, so if
// it survives while the host's bytes do not, the missing page mark is the only
// difference between them.
//
// The flush COUNT is asserted too, and it is the half a byte range could not
// state: three pages, the string's two and the control's one, with pages 2 and 3
// left alone. Under the old min/max the same run reported five, and "five" is
// consistent both with marking correctly and with marking a span that happens to
// cover the right bytes -- so it could not tell this test's two hypotheses apart.
func TestAHostWrittenStringIsMarkedDirty(t *testing.T) {
	const wat = `(module
		(memory 1)
		(func (export "poke") (param $at i32) (param $v i32)
			(i32.store8 (local.get $at) (local.get $v)))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load8_u (local.get $at))))`
	const script = `
-- Stands in for Factorio's storage. Nothing but what lands here survives.
local storage = {}

local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()            -- state_init
local _, size = a.persist.memory()
storage.fk_memsize = size

-- 4090 is unaligned and 20 bytes from there run past 4096, so this exercises
-- the head, the aligned body and the tail, over two packed pages.
a.memio.wstr(4090, "abcdefghijklmnopqrst")
a.exports["poke"](20000, 65)                   -- the control, in page 4

print("dirty " .. a.persist.flush(storage.fk_pages))      -- sync_memory
local _, live = a.persist.memory()
storage.fk_memsize = live

local b = mk({})                               -- the next load
b.persist.restore(storage.fk_pages, storage.fk_memsize)   -- state_load
print("host  " .. b.read_string(4090, 20))
print("guest " .. b.exports["peek"](20000))
`
	want := "dirty 3\nhost  abcdefghijklmnopqrst\nguest 65"
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s: got %q, want %q", lvl, got, want)
		}
	}
}

// A call that writes LOW and HIGH repacks two pages, not everything between.
//
// This is the shape a real mod's host call has and it is what made packed mode
// unusable at scale. The dirty record used to be a min/max byte RANGE, so a
// call touching a static near address zero and a heap object near the top of a
// 1 MiB memory reported every page in between as dirty -- 245 of them here for
// eight bytes actually written. Downstream measured that as a 200-rig create
// taking 447 s against ~15 s in table mode, which is why the first real mod
// shipped the mode with the worse GC profile.
//
// The two values also have to survive the round trip, because "flush fewer
// pages" and "flush the wrong pages" are the same number.
func TestAScatteredWriteRepacksOnlyThePagesItTouched(t *testing.T) {
	const wat = `(module
		(memory 16)
		(func (export "poke") (param $at i32) (param $v i32)
			(i32.store (local.get $at) (local.get $v)))
		(func (export "peek") (param $at i32) (result i32)
			(i32.load (local.get $at))))`
	const script = `
local storage = {}

local a = mk({})
a.persist.arm()
storage.fk_pages = a.persist.pack()            -- state_init
local _, size = a.persist.memory()
storage.fk_memsize = size

a.exports["poke"](16, 1111)                    -- a static, page 0
a.exports["poke"](1000000, 2222)               -- the heap, page 244
print("dirty " .. a.persist.flush(storage.fk_pages))   -- sync_memory

-- Two writes inside ONE page are one page, which is the property that makes the
-- record a set of pages rather than a set of addresses.
a.exports["poke"](600000, 3333)
a.exports["poke"](601000, 4444)
print("same  " .. a.persist.flush(storage.fk_pages))

local b = mk({})                               -- the next load
b.persist.restore(storage.fk_pages, storage.fk_memsize)   -- state_load
print("low   " .. b.exports["peek"](16))
print("high  " .. b.exports["peek"](1000000))
print("mid   " .. b.exports["peek"](600000))
print("mid2  " .. b.exports["peek"](601000))
`
	want := strings.Join([]string{
		"dirty 2",
		"same  1",
		"low   1111",
		"high  2222",
		"mid   3333",
		"mid2  4444",
	}, "\n")
	for _, lvl := range allLevels {
		if got := twoInstancesWith(t, wat, script, lvl, PersistPacked); got != want {
			t.Errorf("-opt=%s:\ngot:\n%s\nwant:\n%s", lvl, got, want)
		}
	}
}

// --fuel is the answer to the one platform limit with no upper bound: wasm has
// no instruction budget, Factorio enforces none, and a mod cannot be
// interrupted. An infinite guest loop hangs every player's client until they
// kill the process, so trapping is the only outcome better than that.
const spinWAT = `(module
	(func (export "spin") (param $n i32) (result i32)
		(local $i i32)
		(loop $top
			(local.set $i (i32.add (local.get $i) (i32.const 1)))
			(br_if $top (i32.lt_u (local.get $i) (local.get $n))))
		(local.get $i)))`

func runFuel(t *testing.T, wat, expr string, fuel int) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatalf("ir: %v", err)
	}
	src, err := EmitModuleWith(im, Options{Opt: analysis.O2, Fuel: fuel})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	out, err := h.RunString("local M = (function(...)\n" + src + "\nend)()\n" +
		"local ok, r = pcall(function() return " + expr + " end)\n" +
		`if ok then print(tostring(r)) else
  print("TRAP " .. tostring(type(r) == "table" and r.fk_trap or r))
end` + "\n")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return strings.TrimSpace(out)
}

func TestFuelStopsARunawayLoop(t *testing.T) {
	// A loop that wants 1,000,000 iterations, given 1,000.
	if got, want := runFuel(t, spinWAT, `M.exports["spin"](1000000)`, 1000),
		"TRAP out of fuel"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The same loop inside its budget runs to completion and returns the right
	// answer -- fuel must not change what a terminating program computes.
	if got, want := runFuel(t, spinWAT, `M.exports["spin"](1000)`, 100000),
		"1000"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// And with the check off, the budget is irrelevant.
	if got, want := runFuel(t, spinWAT, `M.exports["spin"](5000)`, 0),
		"5000"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The budget is per ENTRY CALL, not per session. A mod given one budget for the
// whole game would run fine for an hour and then start trapping in a handler
// that had not changed -- worse than not having the check at all.
func TestFuelIsRefilledAtEveryEntryPoint(t *testing.T) {
	// 600 iterations against a 1000 budget, called three times. Per-call it is
	// comfortable; cumulatively it is 1800 and would trap on the second.
	got := runFuel(t, spinWAT, `(function()
		local a = M.exports["spin"](600)
		local b = M.exports["spin"](600)
		local c = M.exports["spin"](600)
		return a + b + c
	end)()`, 1000)
	if want := "1800"; got != want {
		t.Errorf("got %q, want %q -- the budget did not refill between calls", got, want)
	}
}

// A guest with no loop at all should not pay for the machinery.
func TestFuelCostsNothingWhereThereIsNoLoop(t *testing.T) {
	m, err := wasm.DecodeWAT(`(module (func (export "f") (result i32) (i32.const 1)))`)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	src, err := EmitModuleWith(im, Options{Opt: analysis.O2, Fuel: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, "FUEL = FUEL - 1") {
		t.Error("nothing here can loop, so nothing should be charged")
	}
}

// --persist=auto turns on a proxy the compiler CAN see (declared heap size) for
// something it cannot (write locality). The threshold is therefore a judgement,
// and pinning it means a change to it is a deliberate edit rather than a drift.
func TestAutoPicksByHeapSize(t *testing.T) {
	for _, tc := range []struct {
		heap uint64
		want PersistMode
		why  string
	}{
		{0, PersistTable, "no memory at all -- nothing to persist either way"},
		{64 << 10, PersistTable, "a 64 KiB heap costs ~150 KB of save in table mode"},
		{AutoThresholdBytes - 1, PersistTable, "just below the threshold"},
		{AutoThresholdBytes, PersistPacked, "at the threshold, table would cost ~600 KB per save"},
		{16 << 20, PersistPacked, "a 16 MiB heap in table mode is ~9.6 MB of save"},
	} {
		if got := ResolvePersist(PersistAuto, tc.heap); got != tc.want {
			t.Errorf("heap %d: got %s, want %s (%s)", tc.heap, got, tc.want, tc.why)
		}
	}
	// Auto is the only mode that resolves. An explicit choice is never
	// overridden, however big the heap -- an author who said packed on a tiny
	// heap meant it.
	for _, m := range []PersistMode{PersistNone, PersistTable, PersistPacked} {
		if got := ResolvePersist(m, 1<<30); got != m {
			t.Errorf("%s was overridden to %s", m, got)
		}
	}
}

// PersistAuto must never reach code generation: it is a request, not a mode the
// runtime knows how to drive. control.lua checks for "table" and "packed".
func TestAutoIsNeverEmitted(t *testing.T) {
	src := emitBody(t, statefulWAT, analysis.O2)
	if strings.Contains(src, `mode = "auto"`) {
		t.Errorf("auto reached the chunk:\n%s", src)
	}
}
