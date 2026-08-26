package guest_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
	"github.com/Techrocket9/fklua/internal/factorio"
	"github.com/Techrocket9/fklua/internal/guest"
	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
)

// The Rust half of the collector gates.
//
// gc_test.go, gcpace_test.go and gccap_test.go all enter through gcChunk, which
// builds a GO guest -- so every collector assertion in this package was a
// statement about TinyGo until the Rust collector landed. These are the same
// assertions asked of guest/rust/fkgc, driven through the same Lua preamble, and
// the cross-language ones are asked of BOTH guests in one test so that "the two
// agree" is a diff rather than two separately-recorded numbers.
//
// The harness is deliberately a near-copy of gcBuildChunk rather than a
// refactor of it. The two differ in the build command and in nothing else, and
// keeping them side by side is what makes that visible; a shared helper with a
// language switch would hide exactly the asymmetry a reader needs to check.

// needRustGuest skips unless a Rust wasm toolchain and the Lua oracle are both
// here.
//
// It is the Rust twin of needGuest, and it skips rather than fails for the same
// reason RustAvailable exists: rustc knows wasm32-unknown-unknown as a built-in
// target spec whether or not its rlibs are installed, so "can build" is a
// question only a real compile answers.
func needRustGuest(t *testing.T) *luahost.Host {
	t.Helper()
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	return h
}

// rustGCRun compiles a Rust guest at the given arm and runs a Lua body against
// it.
func rustGCRun(t *testing.T, h *luahost.Host, pkg string, collected bool, body string) string {
	t.Helper()
	src := rustGCChunk(t, pkg, collected) + body
	out, err := h.RunString(src)
	if err != nil {
		t.Fatalf("running rust %s (collected=%v): %v\n%s", pkg, collected, err, out)
	}
	return strings.TrimSpace(out)
}

// rustGCChunks memoises on (package, arm): a cargo build plus an emit is about a
// second and the gates below drive the same guest a dozen ways.
var rustGCChunks sync.Map

func rustGCChunk(t *testing.T, pkg string, collected bool) string {
	t.Helper()
	key := pkg + "|" + strconv.FormatBool(collected)
	if v, ok := rustGCChunks.Load(key); ok {
		return v.(string)
	}
	src := rustGCBuildChunk(t, pkg, collected)
	rustGCChunks.Store(key, src)
	return src
}

func rustGCBuildChunk(t *testing.T, pkg string, collected bool) string {
	t.Helper()
	return gcPreamble(t, rustGCEmit(t, pkg, collected))
}

// rustGCEmit builds one arm of a Rust guest and emits it as a Lua chunk.
//
// SEPARATE TARGET DIRS PER ARM, and that is the stage-C cache lesson rather than
// hygiene: cargo writes both arms to the same artifact path inside a target dir,
// so two arms sharing one hand the second reader whichever wasm was built last
// -- with no error, and with every assertion still passing against the wrong
// module.
func rustGCEmit(t *testing.T, pkg string, collected bool) string {
	t.Helper()
	root := repoRoot(t)
	arm := "leaking"
	build := guest.BuildRust
	if collected {
		arm = "collected"
		build = guest.BuildRustCollected
	}
	out := filepath.Join(t.TempDir(), "cargo-"+arm)
	wasmPath, err := build(filepath.Join(root, "guest", "rust"), pkg, out)
	if err != nil {
		t.Fatalf("building rust %s (collected=%v): %v", pkg, collected, err)
	}
	raw, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range im.Funcs {
		if f.Unsupported != nil {
			t.Fatalf("rust collected=%v: function %q did not compile: %v",
				collected, f.Name, f.Unsupported)
		}
	}
	// THE GC MODE HAS TO FOLLOW THE BUILD, for the reason gc_test.go's copy of
	// this comment gives: emitting a leaking chunk for a collected guest would
	// inline the 8-byte store past the page mark, which is precisely the hole the
	// emitter gate exists to close -- and no assertion here could see it, because
	// the answers stay right for the whole run and the damage is a live object
	// swept later.
	gc := luagen.GCLeaking
	if collected {
		gc = luagen.GCCollected
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{
		Opt: analysis.O3, Persist: luagen.PersistTable, GC: gc})
	if err != nil {
		t.Fatal(err)
	}
	return chunk
}

// gcPreamble wraps an emitted chunk in the driver gc_test.go's guests run
// against: K for the exports, WORDS() for linear memory, and STEP/PACE standing
// in for control.lua's one-shot on_tick with the ticks taken out.
//
// It is the ONE preamble both harnesses use: gcBuildChunk calls this same
// function, so the two languages' guests run under identical driver text by
// construction rather than by a comparison test. (An earlier shape kept two
// copies and a test asserting them equal; sharing the function retired both.)
func gcPreamble(t *testing.T, chunk string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(gcPreambleHead)
	b.WriteString("M = (function(...)\n")
	b.WriteString(chunk)
	b.WriteString("\nend)(IMP)\n")
	b.WriteString(gcPreambleTail)
	return b.String()
}

const gcPreambleHead = `local seen = {}
local M
local function rec(p, n) seen[#seen+1] = M.read_string(p, n) end
local GCPEND = false
local GCS
local IMP = { env = { fk_log = rec, fk_print = rec },
  fk = { gc = function() GCPEND = true if GCS then GCS.arm() end return 0 end } }
`

const gcPreambleTail = `local init = M.exports["_initialize"] if init then init() end
local K = setmetatable({}, {__index = function(_, k) return M.exports[k] end})
local WORDS = function() return M.memio.size() / 4 end
local LOGS = function() return table.concat(seen, "\n") end
GCS = M.persist and M.persist.gc
local GCB, GCC = 0, 0
if M.exports["fk_gc_dirty_base"] then
  GCB = M.exports["fk_gc_dirty_base"]()
  GCC = M.exports["fk_gc_dirty_cap"]()
end
local STEP = function()
  if not GCPEND then return 0 end
  local n = GCS.drain(GCB, GCC)
  local ph = M.exports["fk_gc_step"](n)
  if ph == 1 then GCS.arm() else GCS.disarm() end
  if ph == 0 then GCPEND = false end
  return ph
end
local PACE = function()
  local k = 0
  while STEP() ~= 0 do k = k + 1 end
  return k
end
local COLLECTING = function() return GCPEND end
local STEPBLIND = function()
  if not GCPEND then return 0 end
  local ph = M.exports["fk_gc_step"](0)
  if ph == 1 then GCS.arm() else GCS.disarm() end
  if ph == 0 then GCPEND = false end
  return ph
end
local PACEBLIND = function()
  local k = 0
  while STEPBLIND() ~= 0 do k = k + 1 end
  return k
end
`

// rustModule decodes one arm of a Rust guest, for the tests that ask about the
// MODULE rather than about what it computes.
func rustModule(t *testing.T, pkg string, collected bool) *wasm.Module {
	t.Helper()
	root := repoRoot(t)
	build := guest.BuildRust
	arm := "leaking"
	if collected {
		build = guest.BuildRustCollected
		arm = "collected"
	}
	p, err := build(filepath.Join(root, "guest", "rust"), pkg,
		filepath.Join(t.TempDir(), "cargo-"+arm))
	if err != nil {
		t.Fatalf("building rust %s (collected=%v): %v", pkg, collected, err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := wasm.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// THE ROOT SET WAS THE GO/NO-GO, and this is the measurement it turned on.
//
// CLAUDE.md recorded the blocking objection: "rustc has no equivalent seam, and
// Rust keeps live references in wasm locals that a conservative scan of
// [__global_base, __heap_base) plus the shadow stack cannot see." The premise is
// true and the conclusion does not follow, because a step runs only at an
// outermost dispatch boundary, where there is no guest frame and therefore no
// live local to miss -- see guest/rust/fkgc/src/lib.rs, which is where the whole
// argument lives.
//
// What the argument DEPENDS on is rustc's layout, and that is a fact about a
// compiler rather than about this repo, so it is asserted here rather than
// assumed. Two properties, both load-bearing:
//
//   - The module has exactly ONE mutable global and it holds a stack pointer.
//     A mutable global holding a HEAP pointer would be a root outside linear
//     memory, which nothing in this design scans and nothing could.
//   - The statics are ABOVE the shadow stack: rustc links wasm32 stack-first, so
//     `__stack_pointer`'s initial value is also where the data begins. That is
//     what makes `[__global_base, __heap_base)` one contiguous range with no
//     stack in it, and it is the same shape TinyGo's wasm-unknown.json produces
//     with --stack-first.
//
// If rustc ever moves the stack above the data this test fails, and the failure
// is a COST rather than a soundness bug -- the range would then include a
// megabyte of stale stack, which over-retains. The test says which.
func TestARustGuestsRootRangeIsWhereTheCollectorLooks(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	m := rustModule(t, "gctorture", true)

	var mutable []int
	for i, g := range m.Globals {
		if g.Mutable {
			mutable = append(mutable, i)
		}
	}
	if len(mutable) != 1 {
		t.Fatalf("the module has %d mutable globals, not 1. Every one of them is a "+
			"root the collector cannot scan -- wasm globals are not linear memory. "+
			"A new one holding a heap pointer is a soundness bug; see fkgc's lib.rs",
			len(mutable))
	}
	sp := uint32(m.Globals[mutable[0]].InitBits)
	if len(m.Data) == 0 {
		t.Fatal("the collected guest has no data segment at all, so this test " +
			"cannot locate the statics it is about")
	}
	lo := m.Data[0].Offset
	for _, d := range m.Data {
		if d.Offset < lo {
			lo = d.Offset
		}
	}
	if lo < sp {
		t.Errorf("the statics begin at %d, BELOW the initial stack pointer %d: "+
			"rustc is no longer linking this target stack-first. The root range "+
			"[__global_base, __heap_base) now contains the whole shadow stack, so "+
			"the collector over-retains through stale stack words. Not a soundness "+
			"bug -- a cost -- but fkgc's lib.rs claims otherwise and must be "+
			"corrected", lo, sp)
	}
	if lo != sp {
		t.Logf("statics begin at %d and the stack top is %d: a %d-byte gap, which "+
			"is scanned for nothing", lo, sp, lo-sp)
	}
	t.Logf("stack [0, %d), statics from %d -- one mutable global, holding the "+
		"stack pointer", sp, lo)
}

// THE EXPORTS ARE THE COLLECTOR AS FAR AS THE HOST CAN TELL, and both directions
// of that matter.
//
// A collected build must carry all three or checkGC refuses it -- and would be
// right to, because the flag is not inert: it takes the inlined 8-byte store
// back out of line and emits the barrier's arming surface, so a guest with the
// flag and no exports pays for a collector it does not have.
//
// A leaking build must carry NONE, which is the half that is not obvious in
// Rust. The three exports live in a dependency rlib, so whether they reach the
// cdylib is a linker question rather than a source one; this asserts the answer
// in both arms rather than inferring it from the Cargo.toml.
func TestARustGuestCarriesTheCollectorSurfaceExactlyWhenItHasOne(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	want := factorio.CollectorSurface()
	for _, tc := range []struct {
		collected bool
		present   bool
	}{{true, true}, {false, false}} {
		m := rustModule(t, "gctorture", tc.collected)
		have := map[string]bool{}
		for _, e := range m.Exports {
			have[e.Name] = true
		}
		for _, name := range want {
			if have[name] != tc.present {
				t.Errorf("collected=%v: export %q present=%v, want %v",
					tc.collected, name, have[name], tc.present)
			}
		}
	}
}

// The two arms are DIFFERENT MODULES, which is the stage-C cache lesson stated
// as an assertion.
//
// cargo writes both arms to the same artifact path inside a target directory, so
// a harness that shared one would hand the second reader whichever wasm was
// built last -- with no error, and with every other test in this file still
// passing against the wrong module. The Go side cannot have this bug (tinygo is
// told where to write); this side can, so it is checked.
func TestTheTwoRustArmsAreDifferentModules(t *testing.T) {
	if ok, why := guest.RustAvailable(); !ok {
		t.Skipf("skipping: %s", why)
	}
	leak := rustGCEmit(t, "gctorture", false)
	coll := rustGCEmit(t, "gctorture", true)
	if leak == coll {
		t.Fatal("the leaking and collected arms emitted the identical chunk, so " +
			"one of them was read out of the other's cache and every collector " +
			"assertion in this file is comparing a module with itself")
	}
	if len(coll) <= len(leak) {
		t.Errorf("the collected chunk (%d B) is no larger than the leaking one "+
			"(%d B), which a whole mark-sweep collector should not be", len(coll),
			len(leak))
	}
}

// No example may declare the collector feature, and the reason is Cargo's.
//
// The v2 resolver unifies features across every package built in one invocation,
// so an example that declared `fk/fkgc` in its own Cargo.toml would turn the
// collector on for every OTHER example in a workspace-wide `cargo build` --
// silently, and only for that invocation. Two invocations producing different
// wasm from the same source is the shape of non-determinism CLAUDE.md rules out,
// and it would also make `cargo build` at the workspace root disagree with
// `cargo build -p hello`.
//
// The feature is passed on the command line instead; see
// guest.RustCollectorFeature.
func TestNoRustExampleDeclaresTheCollectorFeature(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "guest", "rust", "examples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "Cargo.toml")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		seen++
		if strings.Contains(string(b), "fkgc") && strings.Contains(string(b), "[features]") {
			t.Errorf("%s declares a feature mentioning fkgc. Cargo's v2 resolver "+
				"unifies features across a workspace build, so this turns the "+
				"collector on for every other example too. Pass --features %s on "+
				"the command line instead", p, guest.RustCollectorFeature)
		}
	}
	if seen == 0 {
		t.Fatal("no example Cargo.toml was read, so this test asserted nothing")
	}
}

// tortureBody is the workload BOTH toolchains' torture guests are driven with,
// and it is one string on purpose.
//
// The corpus-mirror tradition is that the Go example is the spec and the Rust
// one reproduces it; a mirror is only checked if the two are asked the same
// question in the same order, and two copies of a Lua body drift the way
// CLAUDE.md records factorio.Hooks drifting.
const tortureBody = `
local st = K['torture_stat']
local built = K['torture_build'](20000)
local ip = K['torture_interior'](12345)
local op = K['torture_one_past'](777)
local lg = K['torture_large'](40000)
local ls = K['torture_last_slot'](0xA5A5A5)
local before = K['torture_verify']()
K['torture_collect']()
print(string.format('built=%d before=%d after=%d interior=%d interior_want=%d large=%d large_want=%d one_past=%d',
  built, before, K['torture_verify'](), K['torture_interior_read'](), ip,
  K['torture_large_read'](), lg, K['torture_one_past_read']()))
print(string.format('kept=%d believed=%d liveobj=%d', K['torture_kept_bytes'](), st(1), st(5)))
local lsr = K['torture_last_slot_reused']()
print(string.format('last_slot=%d last_slot_want=%d last_slot_reused=%d',
  K['torture_last_slot_read'](), ls, lsr))
local rp = K['torture_repoint'](31337)
K['torture_collect']()
print(string.format('repoint=%d repoint_seen=%d repoint_want=%d',
  rp, K['torture_repoint_verify'](), K['torture_repoint_want']()))
local hv = K['torture_hold'](64, 2048)
K['torture_collect']()
print(string.format('held=%d held_after=%d held_bytes=%d', hv, K['torture_hold_verify'](), K['torture_hold_bytes']()))
K['torture_drop_all']()
K['torture_drop_held']()
K['torture_collect']()
K['torture_collect']()
print(string.format('dropped_live=%d dropped_obj=%d cycles=%d', st(1), st(5), st(3)))
`

// EVERYTHING REACHABLE SURVIVES, asked of the Rust collector.
//
// The mirror of TestTheCollectorKeepsWhatIsReachable, and a differential for the
// same reason it is: a collector that reclaims something live does not produce
// an error -- the memory is still addressable, it is zeroed and handed to
// somebody else, so the only symptom anywhere is a number that moved. The
// leaking arm reclaims nothing, so its checksums are right by construction and
// it is the oracle rather than a second opinion.
func TestTheRustCollectorKeepsWhatIsReachable(t *testing.T) {
	h := needRustGuest(t)
	leak := gcFields(t, rustGCRun(t, h, "gctorture", false, tortureBody))
	coll := gcFields(t, rustGCRun(t, h, "gctorture", true, tortureBody))

	for _, k := range []string{"built", "before", "after", "interior", "large",
		"repoint", "held", "held_after", "last_slot"} {
		if leak[k] != coll[k] {
			t.Errorf("%s: the leaking arm says %d, collected says %d -- the "+
				"collector reclaimed something that was still reachable", k,
				leak[k], coll[k])
		}
	}
	if coll["after"] != coll["before"] {
		t.Errorf("the structure changed across a collection: %d before, %d after",
			coll["before"], coll["after"])
	}
	// An INTERIOR pointer is the only reference to that block, and it points into
	// its middle -- 148 bytes into a 256-byte block, deliberately not
	// granule-aligned. agents/gc.md section 1 requires this to work.
	if coll["interior"] != coll["interior_want"] {
		t.Errorf("a block referenced ONLY through an interior pointer was "+
			"reclaimed: read %d, wrote %d", coll["interior"], coll["interior_want"])
	}
	// ONE PAST THE END: asserted rather than inherited, and the answer is NO,
	// which is the same answer the Go collector gives and the specific reason
	// agents/gc.md's wasip1 gate stays shut.
	if leak["one_past"] != 1 {
		t.Fatalf("the leaking control says a one-past-the-end read does not see "+
			"what was written (%d); the probe is broken, not the collector",
			leak["one_past"])
	}
	if coll["one_past"] != 0 {
		t.Errorf("a one-past-the-end pointer retained its block (%d). That is not "+
			"wrong, but it is a CHANGE, and agents/gc.md's wasip1 gate is argued "+
			"on the other answer", coll["one_past"])
	}
	// THE LAST SLOT OF A SMALLEST-CLASS SPAN, the one slot whose index collided
	// with the "not an object" sentinel in the table mark_candidate resolves a
	// candidate through. Same defect, same shape, same fix as the Go collector's:
	// a 4 KiB span holds exactly 256 sixteen-byte objects, so the last one's slot
	// index is 255 and SLOT_NONE was 255.
	if coll["last_slot_want"] == 0 {
		t.Fatal("the probe never got a block into the last slot of a span, so " +
			"nothing here is a statement about it; the probe is broken, not " +
			"the collector")
	}
	if leak["last_slot_reused"] != 0 {
		t.Fatalf("the leaking control handed the same block out twice (%d); the "+
			"probe is broken, not the collector", leak["last_slot_reused"])
	}
	if coll["last_slot_reused"] != 0 {
		t.Errorf("the block in the LAST slot of a smallest-class span was handed " +
			"to a later allocation while the guest was still holding a " +
			"reference to it")
	}
	if coll["last_slot"] != coll["last_slot_want"] {
		t.Errorf("the block in the LAST slot of a smallest-class span lost what "+
			"was written to it: read %d, wrote %d",
			coll["last_slot"], coll["last_slot_want"])
	}

	// The store into an object the collector had already marked.
	if coll["repoint_seen"] != coll["repoint_want"] {
		t.Errorf("a store into a MARKED object was lost across a collection: the "+
			"guest sees %d and wrote %d. That is the write barrier",
			coll["repoint_seen"], coll["repoint_want"])
	}

	ratio := float64(coll["believed"]) / float64(coll["kept"])
	t.Logf("the guest holds %d B, the collector believes %d B in %d objects -- "+
		"retention %.3fx; one-past-the-end retains: %v", coll["kept"],
		coll["believed"], coll["liveobj"], ratio, coll["one_past"] == 1)
	if ratio > 2 {
		t.Errorf("conservative retention is %.2fx the real live set, against a "+
			"~2x bar: a heap that over-retains this much doubles regardless and "+
			"the collector has bought nothing", ratio)
	}
	// The other direction, which a collector that simply retained everything
	// would pass the above on.
	if coll["dropped_live"] > 64<<10 {
		t.Errorf("after dropping every root the collector still believes %d B in "+
			"%d objects are live; it is retaining rather than collecting",
			coll["dropped_live"], coll["dropped_obj"])
	}
}

// THE MIRROR TABLE: the two collectors, asked the same question, compared as
// numbers.
//
// This is the whole point of porting the corpus rather than writing a new guest.
// Every field below is pure arithmetic over the workload -- a wrapping u32 fold
// of values the guest computed -- so the two languages must agree EXACTLY, and a
// disagreement is a workload that was not actually mirrored or a collector that
// lost something.
//
// Three fields are excluded and named rather than quietly dropped, because each
// exclusion is a fact about the port:
//
//	kept        folds roots.capacity(), and Go's append and Rust's Vec do not
//	            grow on the same curve. It is a heap-accounting probe.
//	believed    the collector's live-byte total, which depends on the size
//	            classes the two languages' structs land in.
//	liveobj     likewise.
//	dropped_*   likewise.
//
// Everything else is a checksum, and checksums are the bar.
func TestTheTwoCollectorsAgreeOnTheTortureCorpus(t *testing.T) {
	if ok, why := guest.Available(); !ok {
		t.Skipf("skipping: %s", why)
	}
	h := needRustGuest(t)
	goOut := gcFields(t, gcRun(t, h, "./examples/gctorture", true, tortureBody))
	rsOut := gcFields(t, rustGCRun(t, h, "gctorture", true, tortureBody))

	checksums := []string{"built", "before", "after", "interior", "interior_want",
		"large", "large_want", "one_past", "repoint", "repoint_seen",
		"repoint_want", "held", "held_after", "held_bytes",
		"last_slot", "last_slot_want", "last_slot_reused"}
	for _, k := range checksums {
		if goOut[k] != rsOut[k] {
			t.Errorf("%s: the Go collector says %d, the Rust collector says %d. "+
				"These are wrapping-u32 folds of the workload and nothing else, "+
				"so either the guests are not running the same workload or one "+
				"collector lost an object the other kept", k, goOut[k], rsOut[k])
		}
	}
	t.Logf("checksums agree across %d fields; heap accounting differs as "+
		"expected -- Go believes %d B in %d objects, Rust %d B in %d",
		len(checksums), goOut["believed"], goOut["liveobj"],
		rsOut["believed"], rsOut["liveobj"])
	// Both must actually have COLLECTED. Two guests that agree because neither
	// reclaimed anything agree about nothing.
	if goOut["cycles"] == 0 || rsOut["cycles"] == 0 {
		t.Errorf("cycles: Go %d, Rust %d -- a comparison between two collectors "+
			"that did not run is not a comparison", goOut["cycles"], rsOut["cycles"])
	}
}

// A STORE INTO A MARKED OBJECT DURING MARKING IS NOT LOST, which is the write
// barrier and the one thing a paced collector has that a stop-the-world one does
// not need.
//
// The mirror of TestAStoreIntoAMarkedObjectDuringMarkingIsNotLost, and it is
// asserted in BOTH directions: PACE drains the dirty page set the way
// runtime/lua/fk_mod.lua does, and PACEBLIND tells the collector nothing was
// written. If the blind arm also passed, the barrier would not be what is
// keeping the object -- the test would be asserting into the void.
func TestAStoreIntoAMarkedRustObjectDuringMarkingIsNotLost(t *testing.T) {
	h := needRustGuest(t)
	body := `
K['torture_gc_budget'](256)
K['torture_build'](4000)
local want = K['torture_repoint'](1)
K['torture_collect']()
-- A fresh paced collection, interrupted halfway, with a store landing in the
-- middle of it: the roots were marked in an earlier step, so the object holding
-- the new reference has already been scanned.
K['torture_gc_start']()
STEP() STEP() STEP()
local mid = K['torture_repoint'](999)
while STEP() ~= 0 do end
print(string.format('seen=%d want=%d mid=%d', K['torture_repoint_verify'](), K['torture_repoint_want'](), mid))
`
	got := gcFields(t, rustGCRun(t, h, "gctorture", true, body))
	if got["seen"] != got["want"] {
		t.Errorf("a store made DURING marking was lost: the guest reads %d and "+
			"wrote %d. The dirty page set is the collector's card table and this "+
			"is what it is for", got["seen"], got["want"])
	}

	blind := `
K['torture_gc_budget'](256)
K['torture_build'](4000)
K['torture_repoint'](1)
K['torture_collect']()
K['torture_gc_start']()
STEPBLIND() STEPBLIND() STEPBLIND()
K['torture_repoint'](999)
while STEPBLIND() ~= 0 do end
print(string.format('seen=%d want=%d', K['torture_repoint_verify'](), K['torture_repoint_want']()))
`
	b := gcFields(t, rustGCRun(t, h, "gctorture", true, blind))
	if b["seen"] == b["want"] {
		t.Errorf("the collector kept the store even though the dirty page set was "+
			"never drained (seen=%d want=%d). Then the barrier is not what is "+
			"keeping it, and the positive half of this test asserts nothing",
			b["seen"], b["want"])
	}
}

// A MARK TERMINATES WHEN THE ROOTS COST MORE THAN THE BUDGET -- the Rust mirror
// of TestAMarkTerminatesWhenTheRootsCostMoreThanTheBudget, whose header carries
// the measurement, the field report and the argument for why the root scan is
// floored rather than made resumable.
//
// It is a mirror rather than a duplicate because the two collectors are separate
// implementations of one design and this defect was in both: the shared shape is
// a termination attempt that walks the whole root range, charges what it walked,
// saturates its budget to zero and then reads that zero as "not finished".
func TestARustMarkTerminatesWhenTheRootsCostMoreThanTheBudget(t *testing.T) {
	h := needRustGuest(t)
	body := `
K['torture_gc_budget'](8)
K['torture_build'](200)
local verify0 = K['torture_verify']()
local started = K['torture_gc_start']()
local ph, steps = 1, 0
-- A hard cap rather than PACE(), because the defect under test is an infinite
-- loop: a bare while-loop would hang the suite instead of failing it.
while ph ~= 0 and steps < 4000 do ph = STEP() steps = steps + 1 end
print(string.format('budget=%d eff=%d started=%d steps=%d phase=%d rootwords=%d '..
  'terms=%d deadlines=%d verify0=%d verify=%d warned=%d',
  K['torture_stat'](11), K['torture_stat'](26), started, steps,
  K['torture_stat'](9), K['torture_stat'](19), K['torture_stat'](18),
  K['torture_stat'](14), verify0, K['torture_verify'](),
  (LOGS():find('ROOT SET') and 1 or 0)))
`
	f := gcFields(t, rustGCRun(t, h, "gctorture", true, body))
	if f["started"] != 1 || f["rootwords"] == 0 {
		t.Fatalf("no collection started (%d) or no root words (%d); nothing below "+
			"means anything", f["started"], f["rootwords"])
	}
	if cost := f["rootwords"] / 4; cost <= f["budget"] {
		t.Fatalf("the root re-scan costs %d granules against a %d-granule budget, "+
			"so this test no longer reproduces the starvation", cost, f["budget"])
	}
	if f["phase"] != 0 {
		t.Errorf("the collector is still in phase %d after %d steps: the mark "+
			"never terminated", f["phase"], f["steps"])
	}
	if f["terms"] > 4 {
		t.Errorf("the mark phase made %d termination attempts over %d steps -- "+
			"each re-walks the whole root range and banks nothing", f["terms"],
			f["steps"])
	}
	if f["deadlines"] != 0 {
		t.Errorf("the mark-termination deadline fired %d times on a guest that is "+
			"not writing anything: the root scan is starving termination again",
			f["deadlines"])
	}
	if f["eff"] <= f["budget"] {
		t.Errorf("effective_budget is %d against a requested %d -- the floor did "+
			"not bind, so whatever terminated the mark was not the fix under test",
			f["eff"], f["budget"])
	}
	if f["warned"] != 1 {
		t.Errorf("the collector raised the budget from %d to %d and logged no "+
			"fkgc: line. Nothing outside the collector can see this condition",
			f["budget"], f["eff"])
	}
	if f["verify"] != f["verify0"] {
		t.Errorf("the retained structure changed across the collection: %d "+
			"before, %d after", f["verify0"], f["verify"])
	}
	t.Logf("%d root words (%d granules) at a %d-granule budget: effective %d, "+
		"%d steps, %d termination attempts, %d deadlines", f["rootwords"],
		f["rootwords"]/4, f["budget"], f["eff"], f["steps"], f["terms"],
		f["deadlines"])
}
