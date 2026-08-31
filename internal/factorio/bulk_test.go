package factorio

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/ir"
	"github.com/Techrocket9/fklua/internal/luagen"
	"github.com/Techrocket9/fklua/internal/luahost"
	"github.com/Techrocket9/fklua/internal/wasm"
	luart "github.com/Techrocket9/fklua/runtime"
)

// THE BULK ATTRIBUTE READ: one attribute off N handles in ONE crossing.
//
// Everything here drives the REAL runtime/lua/fk_abi.lua against a REAL emitted
// memio, because the whole question is whether two ends agree about bytes -- the
// same reason marshal_test.go is written that way. What is asserted above all
// else is the EQUALITY AGAINST THE PER-CALL PATH: a bulk read of N handles must
// leave the destination byte for byte what N separate fk.calls would have left,
// which is what makes the fast arm safe to have at all.

// A module that exists only for its linear memory. One page is 64 KiB, which is
// room for every corpus here and is wholly inside shard 0 -- so the FAST arm is
// what these run on unless a test deliberately breaks its precondition.
const bulkMemWAT = `(module (memory 1)
	(func (export "f") (result i32) (i32.const 0)))`

// ...and a bigger one, for the one test that has to put a handle array ACROSS a
// shard boundary. 33 pages is 2,162,688 bytes and a shard is 2,097,152, so the
// boundary is inside the memory and reachable.
const bulkBigMemWAT = `(module (memory 33)
	(func (export "f") (result i32) (i32.const 0)))`

// The member table these tests dispatch through, written the way LuaSource
// renders one. Four entries, chosen so that every arm of M.bulk_get is
// reachable: a mandatory u32, an OPTIONAL f64 (presence byte at 0, value at 8,
// stride 16), a HANDLE, and a method -- which is what the refusal is about.
const bulkMembers = `
H.bind_members({
  [1] = {kind=H.GET, name="unit_number", class="LuaEntity", valid=true,
         argsize=0, retsize=4, sig={args={}, rets={{kind=H.K_U32, at=0}}}},
  [2] = {kind=H.GET, name="temperature", class="LuaEntity", valid=true, opt=true,
         argsize=0, retsize=16, sig={args={}, rets={{kind=H.K_F64, at=8, has=0}}}},
  [3] = {kind=H.GET, name="surface", class="LuaEntity", valid=true,
         argsize=0, retsize=4, sig={args={}, rets={{kind=H.K_HANDLE, at=0}}}},
  [4] = {kind=H.CALL, name="destroy", class="LuaEntity", valid=true,
         argsize=0, retsize=0, sig={args={}, rets={}}},
})
`

// runBulk instantiates the memory module, binds its memio AND its live shard
// vector into the ABI, installs the member table above, and runs the script.
//
// BINDING THE SHARDS IS WHAT MAKES THE FAST ARM REACHABLE, and it is the same
// call fk_mod.lua makes out of the module's own persist.memory -- a FUNCTION, so
// nothing holds a shard across a grow.
func runBulk(t *testing.T, wat, script string) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	m, err := wasm.DecodeWAT(wat)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ir.BuildModule(m)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fk_abi.lua"), []byte(luart.ABI()), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package.path = " + luaQuote(filepath.Join(dir, "?.lua")) + "\n" +
		"local H = require(\"fk_abi\")\n" +
		"local M = (function(...)\n" + chunk + "\nend)({})\n" +
		"local IO = M.memio\n" +
		"H.bind_memory(IO)\n" +
		"H.bind_read_string(M.read_string)\n" +
		"H.bind_globals({})\n" +
		"H.bind_shards(M.persist.memory)\n" +
		bulkMembers + script
	out, err := h.RunString(src)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	return strings.TrimSpace(out)
}

// The corpus every equality test uses: N entities with distinct values, their
// handles laid out in guest memory exactly as an array return would have left
// them -- 4-byte stride, which is what a []LuaEntity already is.
const bulkCorpus = `
local N = 32
local HP, DST, DST2, RETP = 1024, 4096, 12288, 8192
local ents = {}
for i = 0, N - 1 do
  ents[i] = { valid = true, unit_number = 100 + i, temperature = 20.5 + i }
  IO.st32(HP + i * 4, H.transient(ents[i]))
end
`

// THE CORRECTNESS CENTREPIECE. A bulk read of N handles must leave the
// destination byte for byte what N separate host calls would have left.
//
// It is a BYTE comparison rather than a value one on purpose: the destination is
// an array of the getter's own return block, so "the same bytes" is the whole
// claim -- element i lives at dstp + i*retsize and is what fk.call would have
// written at retp. A comparison of decoded numbers would pass over a stride that
// was wrong by a padding byte.
func TestABulkReadFillsEveryElement(t *testing.T) {
	// BOTH STRIDES, because a fast arm that stepped by a constant 4 instead of by
	// the member's own retsize would be invisible on the first: the mandatory u32
	// block IS four bytes. The optional f64 is sixteen, so element 1 lands three
	// quarters of a block early and the comparison fails there.
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
ents[2].temperature = nil
for _, mid in ipairs({1, 2}) do
  local stride = mid == 1 and 4 or 16
  -- BOTH DESTINATIONS ZEROED FIRST, which is the per-call path's own premise:
  -- encode_rets writes nothing at all for an absent optional because "the value
  -- slot is the guest's own zeroed memory". A bulk destination is a slice the
  -- guest REUSES, so this one writes the zero rather than assuming it -- and
  -- comparing against a per-call run over dirty memory would be comparing
  -- against a block the per-call path never promised anything about.
  for b = 0, N * stride - 1 do IO.st8(DST + b, 0) IO.st8(DST2 + b, 0) end
  for i = 0, N - 1 do
    local st = H.call(IO.ld32(HP + i * 4), mid, 0, DST2 + i * stride)
    if st ~= H.OK then print("per-call " .. i .. " st " .. st) end
  end
  local st = H.bulk_get(mid, HP, N, DST, RETP)
  print("member " .. mid .. " status " .. st .. " read " .. IO.ld32(RETP))
  local bad = -1
  for b = 0, N * stride - 1 do
    if IO.ld8(DST + b) ~= IO.ld8(DST2 + b) then bad = b break end
  end
  print("first differing byte " .. bad)
  if mid == 1 then
    print("first " .. IO.ld32(DST) .. " last " .. IO.ld32(DST + (N - 1) * 4))
  end
end
`)
	want := "member 1 status 0 read 32\nfirst differing byte -1\nfirst 100 last 131\n" +
		"member 2 status 0 read 32\nfirst differing byte -1"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a bulk read has to leave exactly what the "+
			"per-call path leaves; anything else and the guest's own decoder is "+
			"reading a block this wrote differently)", got, want)
	}
}

// THE TWO ARMS ANSWER THE SAME THING, and the general one is reached by breaking
// the fast one's precondition rather than by a flag -- which is the property
// worth having, since that is how it is reached in the field.
//
// An UNALIGNED handle array is the lever here: the fast arm needs a 4-aligned
// contiguous run, and one byte of offset is enough to send the same corpus down
// the accessor-per-element path.
func TestTheFastAndGeneralArmsAgree(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
-- The same handles again, one byte off, which is the fast arm's precondition
-- broken and nothing else.
for i = 0, N - 1 do IO.st8(2049 + i * 4, 0) end
for i = 0, N - 1 do
  local v = IO.ld32(HP + i * 4)
  for b = 0, 3 do
    IO.st8(2049 + i * 4 + b, math.floor(v / (256 ^ b)) % 256)
  end
end
local a = H.bulk_get(1, HP, N, DST, RETP)
local ra = IO.ld32(RETP)
local b = H.bulk_get(1, 2049, N, DST2, RETP)
local rb = IO.ld32(RETP)
print("fast " .. a .. "/" .. ra .. " general " .. b .. "/" .. rb)
local bad = -1
for i = 0, N * 4 - 1 do
  if IO.ld8(DST + i) ~= IO.ld8(DST2 + i) then bad = i break end
end
print("first differing byte " .. bad)
`)
	want := "fast 0/32 general 0/32\nfirst differing byte -1"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A DEAD HANDLE CLEARS ITS ELEMENT AND THE WALK CONTINUES, which is the error
// rule and is the opposite of the batched GUI add's. The elements are
// independent here, and a poll over a thousand entities of which one died
// between the search and the read is the ORDINARY case rather than an error.
//
// The element is written as the ZERO rather than left alone, which is what stops
// a reused destination handing back the PREVIOUS crossing's value -- the
// plausible wrong answer, and the worst one available.
func TestADeadHandleClearsItsElementAndTheWalkContinues(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
-- Fill the destination with a recognisable previous crossing.
for i = 0, N - 1 do IO.st32(DST + i * 4, 999) end
IO.st32(HP + 7 * 4, 0)                       -- element 7's handle is now dead
local st = H.bulk_get(1, HP, N, DST, RETP)
print("status " .. st .. " read " .. IO.ld32(RETP))
print("6=" .. IO.ld32(DST + 6 * 4) .. " 7=" .. IO.ld32(DST + 7 * 4) ..
      " 8=" .. IO.ld32(DST + 8 * 4))
`)
	want := "status 0 read 31\n6=106 7=0 8=108"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(a dead handle must cost its own element "+
			"and nothing else, and must not leave the previous value standing)", got, want)
	}
}

// AN INVALID OBJECT IS SKIPPED RATHER THAN FAILING THE CALL, which is the same
// rule one condition over: Factorio invalidates a LuaObject when the thing
// behind it is destroyed, and a search's results going stale between the search
// and the read is what a poll is FOR.
func TestAnInvalidObjectIsSkippedRatherThanFailingTheCall(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
ents[3].valid = false
local st = H.bulk_get(1, HP, N, DST, RETP)
print("status " .. st .. " read " .. IO.ld32(RETP))
print("2=" .. IO.ld32(DST + 2 * 4) .. " 3=" .. IO.ld32(DST + 3 * 4) ..
      " 4=" .. IO.ld32(DST + 4 * 4))
`)
	want := "status 0 read 31\n2=102 3=0 4=104"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A RAISE AT ELEMENT i DOES NOT ABANDON THE BATCH, and last_error names it.
//
// A Factorio LuaObject's __index really does raise -- reading a key a class does
// not have is the trap the `valid` probe was built around -- so this is the
// engine's own behaviour rather than a hypothetical, and the pcall per element
// is what the error rule costs.
func TestARaisingAttributeDoesNotAbandonTheBatch(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
ents[5] = setmetatable({}, { __index = function(_, k)
  if k == "valid" then return true end
  error("LuaEntity doesn't contain key " .. k)
end })
IO.st32(HP + 5 * 4, H.transient(ents[5]))
local st = H.bulk_get(1, HP, N, DST, RETP)
print("status " .. st .. " read " .. IO.ld32(RETP))
print("4=" .. IO.ld32(DST + 4 * 4) .. " 5=" .. IO.ld32(DST + 5 * 4) ..
      " 6=" .. IO.ld32(DST + 6 * 4))
print("said " .. (string.find(H.last_error(), "unit_number", 1, true) ~= nil
      and "the member" or H.last_error()))
`)
	want := "status 0 read 31\n4=104 5=0 6=106\nsaid the member"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// AN OPTIONAL ATTRIBUTE'S PRESENCE BYTE IS THE ONE PLACE ABSENT AND PRESENT ARE
// DISTINGUISHABLE, and the bulk form writes it exactly where the single getter
// would: at dstp + i*retsize + has.
//
// It also pins the STRIDE, which is the thing a wrong fast arm gets wrong: an
// optional f64 return block is 16 bytes -- presence at 0, value at 8 -- and a
// reader that assumed 8 would find element 1's presence byte in element 0's
// value.
func TestAnOptionalAttributeClearsItsPresenceByte(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
ents[2].temperature = nil                    -- present on a reactor, absent on a chest
local st = H.bulk_get(2, HP, N, DST, RETP)
print("status " .. st .. " read " .. IO.ld32(RETP))
for _, i in ipairs({1, 2, 3}) do
  print(i .. " has=" .. IO.ld8(DST + i * 16) .. " v=" .. IO.ldf64(DST + i * 16 + 8))
end
`)
	// Element 2 is READ successfully and its value is absent, so it counts: an
	// optional attribute saying nothing is an answer rather than a failure.
	want := "status 0 read 32\n1 has=1 v=21.5\n2 has=0 v=0\n3 has=1 v=23.5"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A HANDLE-RETURNING ATTRIBUTE WORKS WITH NO SPECIAL CASE, because minting a
// transient handle is what the single getter's own encode does -- and they are
// TRANSIENT, so they die with the dispatch like every other one.
func TestABulkHandleReturnMintsTransientHandles(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
local surf = { valid = true, object_name = "LuaSurface" }
for i = 0, N - 1 do ents[i].surface = surf end
local _, before = H.stats()
local st = H.bulk_get(3, HP, N, DST, RETP)
local _, after = H.stats()
print("status " .. st .. " read " .. IO.ld32(RETP))
print("minted " .. (after - before))
print("resolves " .. tostring((H.get(IO.ld32(DST + 9 * 4))) == surf))
H.clear_transient()
local _, cleared = H.stats()
print("after clear " .. cleared)
`)
	want := "status 0 read 32\nminted 32\nresolves true\nafter clear 0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A MEMBER THAT IS NOT A READABLE ATTRIBUTE OF AN ELIGIBLE SHAPE IS REFUSED, and
// the status is about the CALL rather than about any element.
//
// The three refusals are one sentence each: a method has arguments this form has
// nowhere to put; a member id nothing bound is the ordinary degradation every
// other kind gets; and one of the five UNFILLABLE handlers is in the HOST table
// and bound by no guest -- so a bulk read naming its id must refuse cleanly
// rather than reading `on_nth_tick` off an entity.
func TestABulkReadRefusesAMemberThatIsNotAReadableAttribute(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
print("method " .. H.bulk_get(4, HP, N, DST, RETP))
print("absent " .. H.bulk_get(99, HP, N, DST, RETP))
-- A member whose single return is a STRING: eligible-looking, and a (ptr, len)
-- into the scratch region is not a flat array element.
H.bind_members({[1] = {kind=H.GET, name="name", class="LuaEntity", valid=true,
  argsize=0, retsize=8, sig={args={}, rets={{kind=H.K_STR, at=0}}}}})
print("string " .. H.bulk_get(1, HP, N, DST, RETP))
`)
	want := "method 3\nabsent 3\nstring 3" // ERR_NO_MEMBER, three times
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A DESTINATION THAT CANNOT HOLD count ELEMENTS IS ERR_BAD_ARGS, checked ONCE
// and up front rather than trapping halfway through a block it has already half
// written.
//
// A trap would be defensible -- it is what a guest's own out-of-bounds store
// does -- and it is the wrong answer here for one reason: the fast arm has
// already written elements 0..k by the time it reaches the end of the memory, so
// a trap would leave a partially written destination and no status to say so.
func TestABulkReadRefusesADestinationThatCannotHoldIt(t *testing.T) {
	got := runBulk(t, bulkMemWAT, bulkCorpus+`
local top = IO.size()
print("dst " .. H.bulk_get(1, HP, N, top - 8, RETP))
print("src " .. H.bulk_get(1, top - 8, N, DST, RETP))
print("count " .. H.bulk_get(1, HP, -1, DST, RETP))
print("retp " .. H.bulk_get(1, HP, N, DST, top - 2))
-- ...and the zero-length call is not an error: it reads nothing and says so.
print("empty " .. H.bulk_get(1, HP, 0, DST, RETP) .. "/" .. IO.ld32(RETP))
`)
	// 4 is ERR_BAD_ARGS and 0 is OK -- fk_abi.lua's own numbers, written out
	// here rather than reached for through a Go constant, because what this
	// asserts is what the RUNTIME answered.
	want := "dst 4\nsrc 4\ncount 4\nretp 4\nempty 0/0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A HANDLE ARRAY THAT STRADDLES A SHARD BOUNDARY TAKES THE GENERAL ARM.
//
// THE FAST ARM'S ONE SOUNDNESS PRECONDITION. Linear memory is a vector of
// 2^19-word shards, and the fast arm indexes shard 0 directly -- so a run that
// crosses the boundary would index that shard past its end, which in Lua is nil
// rather than an error. THE ASSERTION IS ON THE RESULT and not on the absence of
// a raise, because nil handles resolve to nothing and every element past the
// boundary would come back cleared with the call still reporting OK.
func TestAStraddlingBulkSourceTakesTheGeneralArm(t *testing.T) {
	got := runBulk(t, bulkBigMemWAT, `
local N = 8
local SHARD0 = 2097152
local HP = SHARD0 - 16                      -- four handles below, four above
local DST, RETP = 1024, 2048
local ents = {}
for i = 0, N - 1 do
  ents[i] = { valid = true, unit_number = 700 + i }
  IO.st32(HP + i * 4, H.transient(ents[i]))
end
local st = H.bulk_get(1, HP, N, DST, RETP)
print("status " .. st .. " read " .. IO.ld32(RETP))
local vals = {}
for i = 0, N - 1 do vals[#vals + 1] = IO.ld32(DST + i * 4) end
print(table.concat(vals, " "))
`)
	want := "status 0 read 8\n700 701 702 703 704 705 706 707"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the fast arm's shard-0 conjunct is what "+
			"keeps this right; without it everything from the boundary on reads "+
			"as a dead handle)", got, want)
	}
}

// THE ELIGIBLE KIND SET IS SPELLED IN TWO PLACES AND THIS IS THE GATE.
//
// fk_abi.lua's BULKST decides what the RUNTIME will do and BulkEligible decides
// what the GENERATORS emit, and a disagreement is not a compile error in either
// direction: a kind the generators emit and the runtime refuses is a green
// binding that answers ERR_NO_MEMBER, and one the runtime accepts and the
// generators skip is a missing binding nobody notices. Both sides derive their
// set rather than listing it -- the Lua from its own dispatch table and the Go
// from HostAllocatesFor -- so what this compares is two derivations of one fact.
func TestTheBulkKindsAgreeInBothSpellings(t *testing.T) {
	got := runBulk(t, bulkMemWAT, `
print(table.concat(H.bulk_kinds(), ","))
`)
	var want []string
	for _, k := range BulkKinds() {
		want = append(want, fmt.Sprint(int(k)))
	}
	sort.Strings(want)
	gotParts := strings.Split(got, ",")
	sort.Strings(gotParts)
	if strings.Join(gotParts, ",") != strings.Join(want, ",") {
		t.Errorf("fk_abi.lua's BULKST covers kinds %s and BulkEligible admits %s\n"+
			"(a kind on one side only is a binding that always fails or a binding "+
			"that never exists)", got, strings.Join(want, ","))
	}
}

// THE SHARD BOUND IS SPELLED IN TWO RUNTIME FILES AND THIS IS THAT GATE.
//
// fk_rt.lua owns the number -- it is the emitted access's own merge, "a shard is
// 524,288 WHOLE words" -- and fk_abi.lua cannot reach the generated chunk's
// constants, so it carries its own copy for the fast arm's precondition. A copy
// that drifted low would only cost the general arm; one that drifted HIGH would
// index shard 0 past its end, which is the defect the straddle test is about.
func TestTheShardBoundIsSpelledTheSameInBothRuntimes(t *testing.T) {
	abi := regexp.MustCompile(`local SHARD0 = (\d+)`).FindStringSubmatch(luart.ABI())
	if abi == nil {
		t.Fatal("fk_abi.lua no longer declares SHARD0; the fast arm's precondition " +
			"has moved and this gate cannot see where")
	}
	rt := luart.Prelude()
	if !strings.Contains(rt, "if a < 2097152 then return mem[1][a / 4 + 1] end") {
		t.Fatal("fk_rt.lua's ld32 no longer merges the shard-0 case at 2097152; " +
			"re-derive fk_abi.lua's SHARD0 from wherever it moved to")
	}
	if abi[1] != "2097152" {
		t.Errorf("fk_abi.lua says SHARD0 = %s and fk_rt.lua's ld32 merges at "+
			"2097152", abi[1])
	}
}

// BOTH BACKENDS BIND THE SAME BULK VARIANTS, at every committed description.
//
// ONE TEST OVER BOTH, which is the AD5 shape: a hole in one backend is invisible
// to a per-backend test the other also passes. The counts and the POPULATION are
// both compared -- the count alone would pass a pair that emitted the same number
// of different members.
func TestBothBackendsBindTheSameBulkVariants(t *testing.T) {
	for _, v := range committedVersions(t) {
		a, err := LoadAPI(filepath.Join("..", "..", "api", v, "runtime-api.json"))
		if err != nil {
			t.Fatal(err)
		}
		r := GenerateMembers(a)
		ev := GenerateEvents(a)
		g, err := GenerateGoWith(a, r, ev, "fkapi")
		if err != nil {
			t.Fatal(err)
		}
		rb, err := GenerateRust(a, r, ev)
		if err != nil {
			t.Fatal(err)
		}
		if g.BulkVariants != rb.BulkVariants {
			t.Errorf("%s: go emitted %d bulk variants and rust %d", v,
				g.BulkVariants, rb.BulkVariants)
		}
		if g.BulkVariants == 0 {
			t.Fatalf("%s: neither backend emitted a bulk variant, so this test "+
				"audited nothing", v)
		}
		// ...and they are over the same MEMBERS. Both files name the member id in
		// the call, so counting the ids each one bound is a comparison of the
		// populations rather than of two totals that happen to agree.
		goIDs := bulkIDs(t, g.Source, `hostBulkGet\((\d+),`)
		rsIDs := bulkIDs(t, rb.Source, `fk_bulk_get\((\d+),`)
		if len(goIDs) != len(rsIDs) {
			t.Errorf("%s: go bulk-reads %d distinct members and rust %d", v,
				len(goIDs), len(rsIDs))
		}
		for id := range goIDs {
			if !rsIDs[id] {
				t.Errorf("%s: member %s has a Go bulk variant and no Rust one", v, id)
			}
		}
		// The census row is the Go count, and this is what says the Rust one is
		// not written down a second time.
		c, err := TakeCensus(a)
		if err != nil {
			t.Fatal(err)
		}
		if c.BulkVariantBindings != g.BulkVariants {
			t.Errorf("%s: census says %d bulk variants and the generator emitted %d",
				v, c.BulkVariantBindings, g.BulkVariants)
		}
		if c.BulkReadMembers >= c.BulkVariantBindings {
			t.Errorf("%s: %d eligible members and %d bindings -- the bindings must "+
				"outnumber the members by the INHERITED re-renderings", v,
				c.BulkReadMembers, c.BulkVariantBindings)
		}
	}
}

func bulkIDs(t *testing.T, src, pattern string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

// THE PRUNING ASSERTION, and it is the whole reason this is an IMPORT.
//
// The R6 failure shape is a pruning scan defeated by a call-site detail, and the
// alternative design -- one bulk member with the target id inside the ARGUMENT
// BLOCK -- is exactly that: an i32.const stored to memory is not an operand of an
// import, so a guest reading only in bulk would ship all 4,268 members and
// nothing would say so. Here the id is operand 0 of a call to fk.bulk_get, which
// is the shape usedIDs was built for.
//
// A GUEST THAT MAKES ONE BULK READ AND NOTHING ELSE MUST PRUNE TO ONE MEMBER.
func TestTheBulkMemberIDSurvivesTheGeneratedWrapper(t *testing.T) {
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	id := full.MemberIndex()[fmt.Sprintf("LuaEntity::unit_number/%d", MemberGet)]
	if id == 0 {
		t.Fatal("expected LuaEntity::unit_number as a readable attribute")
	}
	wat := fmt.Sprintf(`(module
		(import "fk" "bulk_get" (func $bulk (param i32 i32 i32 i32 i32) (result i32)))
		(memory 1)
		(func (export "fk_on_tick") (param i32)
			(i32.store (i32.const 4096)
				(call $bulk (i32.const %d) (i32.const 1024) (i32.const 8)
					(i32.const 2048) (i32.const 4100)))))`, id)
	im := buildIR(t, wat)
	used, complete := UsedMembers(im)
	if !complete || len(used) != 1 || !used[id] {
		t.Fatalf("the scan found %v (complete=%v); want exactly member %d -- an id "+
			"the pruner cannot see is a mod that ships the whole API", used,
			complete, id)
	}
	pruned := full.Only(used)
	src, err := pruned.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned.Members) != 1 {
		t.Errorf("the pruned table holds %d members, want 1", len(pruned.Members))
	}
	t.Logf("pruned member table: %d bytes for %d members of %d",
		len(src), len(pruned.Members), len(full.Members))
}

// ...AND `api check` SEES IT, which is the same claim one command over: the
// version checker reads UsedMembers, so a guest whose only API contact is a bulk
// read must report a COMPLETE surface naming that member rather than an empty
// one it would then call clean.
func TestAPIcheckSeesABulkOnlyGuest(t *testing.T) {
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	id := full.MemberIndex()[fmt.Sprintf("LuaEntity::unit_number/%d", MemberGet)]
	wat := fmt.Sprintf(`(module
		(import "fk" "bulk_get" (func $bulk (param i32 i32 i32 i32 i32) (result i32)))
		(memory 1)
		(func (export "fk_on_tick") (param i32)
			(i32.store (i32.const 4096)
				(call $bulk (i32.const %d) (i32.const 1024) (i32.const 8)
					(i32.const 2048) (i32.const 4100)))))`, id)
	im := buildIR(t, wat)
	used, complete := UsedMembers(im)
	ev := GenerateEvents(a)
	usedEv, evComplete := UsedEvents(im)
	usedDef, defComplete := UsedDefines(im)
	s := SurfaceOf(full, used, complete, usedEv, evComplete, ev, usedDef,
		defComplete, full.Defines)
	if !s.Complete {
		t.Error("the surface reports incomplete for a guest whose every id is a " +
			"constant, so api check would decline to answer about it")
	}
	if len(s.Members) != 1 || s.Members[0] != "LuaEntity::unit_number" {
		t.Errorf("surface members %v, want exactly LuaEntity::unit_number -- a "+
			"bulk-only guest whose surface reads empty would be called CLEAN by "+
			"api check on a version that removed the member it reads", s.Members)
	}
}

// AND IT REACHES THE ENGINE, through the real control.lua, the real fk_mod.lua
// import table and the real member table -- which is the one leg that says the
// import is WIRED rather than merely written.
//
// Every unit above binds fk_abi.lua by hand. This packages a mod and lets
// Factorio's own require chain do it, so a bulk read that worked in isolation and
// was never bound in fk_mod.lua would fail here and nowhere else.
func TestABulkReadReachesTheEngineThroughAPackagedMod(t *testing.T) {
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	a := loadTestAPI(t)
	full := GenerateMembers(a)
	findID := full.MemberIndex()[fmt.Sprintf("LuaSurface::find_entities_filtered/%d",
		MemberCall)]
	unitID := full.MemberIndex()[fmt.Sprintf("LuaEntity::unit_number/%d", MemberGet)]
	if findID == 0 || unitID == 0 {
		t.Fatal("expected find_entities_filtered and unit_number")
	}
	// Offsets derived rather than assumed: the search writes (ptr, count) of a
	// handle array into its return block, and THAT ARRAY IS THE BULK READ'S
	// INPUT with no marshalling in between -- which is the design's own claim
	// about why the destination and the source line up, made here as a test.
	_, findRets, err := full.Members[findID-1].blocks()
	if err != nil {
		t.Fatal(err)
	}
	findArgs, _, err := full.Members[findID-1].blocks()
	if err != nil {
		t.Fatal(err)
	}
	filtAt := findArgs.Fields[0].Offset
	arrAt := findRets.Fields[0].Offset

	// A REAL fk_alloc, because the search RETURNS A CONTAINER and the host has to
	// put the handle array somewhere the guest owns. A bump allocator over the top
	// of the page is the whole of it -- nothing here frees.
	wat := fmt.Sprintf(`(module
		(import "fk" "call" (func $call (param i32 i32 i32 i32) (result i32)))
		(import "fk" "bulk_get" (func $bulk (param i32 i32 i32 i32 i32) (result i32)))
		(memory 1)
		(global $bump (mut i32) (i32.const 32768))
		(func (export "fk_alloc") (param i32) (result i32)
			(local $p i32)
			(local.set $p (global.get $bump))
			(global.set $bump (i32.add (global.get $bump) (local.get 0)))
			(local.get $p))
		(func (export "fk_free") (param i32))
		(func (export "fk_on_tick") (param i32)
			;; an empty filter array, which is a (ptr, count) of (0, 0)
			(i32.store (i32.const %d) (i32.const 0))
			(i32.store (i32.const %d) (i32.const 0))
			(i32.store (i32.const 4096)
				(call $call (i32.const 2) (i32.const %d) (i32.const 1024)
					(i32.const 512)))
			;; the handle array the search wrote, read in bulk with no copy
			(i32.store (i32.const 4104)
				(call $bulk (i32.const %d) (i32.load (i32.const %d))
					(i32.load (i32.const %d)) (i32.const 8192) (i32.const 4100)))))`,
		1024+filtAt, 1024+filtAt+4, findID, unitID, 512+arrAt, 512+arrAt+4)

	im := buildIR(t, wat)
	used, complete := UsedMembers(im)
	if !complete || len(used) != 2 {
		t.Fatalf("the scan found %v (complete=%v); want the search and the "+
			"attribute", used, complete)
	}
	pruned := full.Only(used)
	apiSrc, err := pruned.LuaSource(a)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := luagen.EmitModuleWith(im, luagen.Options{Opt: 2,
		Persist: luagen.PersistTable})
	if err != nil {
		t.Fatal(err)
	}
	pkg := &Package{
		Info: Info{Name: "fk-bulk", Version: "0.1.0", Title: "t", Author: "x",
			FactorioVersion: DefaultFactorioVersion},
		Chunk: chunk, Exports: []string{"fk_on_tick"}, APITable: apiSrc,
	}
	dir, err := pkg.WriteDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.RunString(fmt.Sprintf(`
package.path = %q
function log(s) end
defines = { events = { on_tick = 1 } }
storage = {}
local handlers = {}
script = {
  mod_name = "fk-bulk",
  on_init = function(f) handlers.on_init = f end,
  on_load = function(f) handlers.on_load = f end,
  on_configuration_changed = function(f) handlers.on_config = f end,
  on_event = function(ev, f) handlers[ev] = f end,
}
-- An engine-shaped surface handing back engine-shaped entities: methods come
-- off __index already bound, which is how Factorio builds one.
local ents = {}
for i = 1, 6 do
  ents[i] = { valid = true, object_name = "LuaEntity", unit_number = 500 + i }
end
-- ENGINE-SHAPED: a method comes off __index as a closure ALREADY BOUND to the
-- object, which is the shape a plain function in a table hid an arity defect
-- behind for a milestone.
local fields = { valid = true, object_name = "LuaGameScript" }
game = setmetatable({}, { __index = function(_, k)
  if fields[k] ~= nil then return fields[k] end
  if k == "find_entities_filtered" then
    return function(_) return ents end
  end
  error("LuaGameScript doesn't contain key " .. tostring(k))
end })
require("control")
handlers[1]({ tick = 1 })
local m = storage.fk_mem[1]
print("search st " .. tostring(m[1025]) .. " bulk st " .. tostring(m[1027]))
print("read " .. tostring(m[1026]))
-- unit_number is an OPTIONAL u64, so an element is 12 bytes: presence at 0,
-- the low word at 4, the high word at 8. Walking it at that stride is the
-- destination layout stated as an assertion.
local vals = {}
for i = 0, 5 do
  local w = 2049 + i * 3
  vals[#vals + 1] = tostring(m[w]) .. ":" .. tostring(m[w + 1]) ..
                    ":" .. tostring(m[w + 2])
end
print(table.concat(vals, " "))
`, filepath.Join(dir, "?.lua")))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "search st 0 bulk st 0\nread 6\n" +
		"1:501:0 1:502:0 1:503:0 1:504:0 1:505:0 1:506:0"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("got:\n%s\nwant:\n%s\n(the handle array a search writes IS the "+
			"array a bulk read consumes; nothing marshals in between)", got, want)
	}
	t.Logf("pruned member table: %d bytes for %d members of %d",
		len(apiSrc), len(pruned.Members), len(full.Members))
}
