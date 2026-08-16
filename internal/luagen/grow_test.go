package luagen

// WHAT `memory.grow` COSTS, AND THE FILL CURSOR THAT MAKES IT NOT COST IT.
//
// mem_grow writes a zero into every new word, at ~107 ns a word in Factorio's
// Lua and with no fixed cost to amortise against (scripts/run-growprobe.sh).
// Since sharding stage C paced the collector that was the worst tick a growing
// guest had, by two orders of magnitude: 22.7-30.0 ms at a 3.5 MiB heap and
// 288-365 ms at 40 MiB, against a collector step of 1.2x a 0.5 ms budget.
//
// The runtime now keeps a materialisation cursor AHEAD of MEMSIZE and advances
// it in paced pieces, so a grow into pre-built words does nothing but move the
// bound. What has to be proved here is not the speed -- bin/lua52f is the wrong
// instrument for what a Factorio table costs, in both directions -- but the two
// things the speed depends on and the three the speed must not have broken:
//
//	the cursor really does run ahead of MEMSIZE     (#MEM[k] past the size)
//	a grow into it creates no slots                 (#MEM[k] does not move)
//	pre-built words are still OUT OF BOUNDS         (a load at MEMSIZE traps)
//	pre-built words read as ZERO once grown into    (memory.grow's contract)
//	a guest that never grows pre-builds NOTHING     (the whole idle-cost claim)
//
// `#MEM[k]` IS THE INSTRUMENT, and it is an honest one rather than a proxy: a
// shard is filled contiguously from index 1, so the length operator is exactly
// "how many words of this shard have been materialised". Nothing else in the
// system can see the difference between a slot that exists holding zero and a
// slot that does not exist, which is the whole reason the cursor is invisible.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/analysis"
)

// growWAT declares ONE page and a max of 2,000, so every word past the first
// 16,384 arrives through mem_grow. The exports are the minimum needed to ask
// the five questions above.
const growWAT = `(module
	(memory 1 2000)
	(func (export "s32") (param i32) (param i32) (i32.store (local.get 0) (local.get 1)))
	(func (export "l32") (param i32) (result i32) (i32.load (local.get 0)))
	(func (export "size") (result i32) (memory.size))
	(func (export "grow") (param i32) (result i32) (memory.grow (local.get 0))))`

// shardLen is the Lua expression for "how many words of the shard holding word
// w have been materialised".
const growPrelude = `local E = M.exports
local P = M.persist
local out = {}
local function shardlen(k) local m = select(1, P.memory()) local t = m[k + 1] return t and #t or 0 end
local function size() return select(2, P.memory()) end
-- Drain the pre-build the way control.lua's one-shot on_tick does: bounded
-- pieces until it says nothing is owed. The cap is a runaway guard, not a
-- budget -- a prebuild that never reports done is the bug this would otherwise
-- hang on.
local function drain(budget)
  local n = 0
  while P.prebuild(budget) do
    n = n + 1
    if n > 10000 then error("prebuild never finished", 0) end
  end
  return n
end
`

func growExpr(body string) string {
	return "(function()\n" + growPrelude + body +
		"\nreturn table.concat(out, \" \")\nend)()"
}

// THE CURSOR RUNS AHEAD OF THE SIZE, and control.lua is TOLD to run it.
//
// Two halves, and both matter. The hook is what makes the pacing possible at
// all -- without it nothing would ever call prebuild and the cursor would never
// move -- and it must fire on a real grow and NOT on the chunk's own initial
// construction, which goes through the same function with size = 0.
func TestAGrowArmsAPreBuildAheadOfTheSizeTheGuestCanSee(t *testing.T) {
	body := `
local armed = 0
P.grow_hook(function() armed = armed + 1 end)
out[#out+1] = "armed_before_any_grow=" .. armed
-- One page. 16,384 words, so the shard holding them is materialised to 32,768.
E.grow(1)
out[#out+1] = "armed_after=" .. armed
out[#out+1] = "size=" .. size()
out[#out+1] = "len_before_drain=" .. shardlen(0)
local steps = drain(4096)
out[#out+1] = "steps=" .. steps
out[#out+1] = "len_after_drain=" .. shardlen(0)
`
	want := strings.Join([]string{
		"armed_before_any_grow=0",
		"armed_after=1",
		"size=131072",
		// The grow itself materialises exactly what it handed over.
		"len_before_drain=32768",
		// 16,384 words of lookahead in 4,096-word pieces is four calls, and
		// drain counts the ones that reported MORE OWED -- the fourth reaches
		// the target and returns false, which is what ends the loop.
		"steps=3",
		"len_after_drain=49152",
	}, " ")
	sameAtEveryLevel(t, growWAT, growExpr(body), want)
}

// A GROW INTO PRE-BUILT WORDS CREATES NO SLOTS. This is the whole win, stated
// as the only property that can be observed without a clock.
func TestAGrowIntoPreBuiltWordsMaterialisesNothing(t *testing.T) {
	body := `
E.grow(1)
drain(4096)
local before = shardlen(0)
E.grow(1)
out[#out+1] = "len_unchanged=" .. tostring(shardlen(0) == before)
out[#out+1] = "size=" .. size()
-- And the words it handed over are ZERO and addressable, which is what
-- memory.grow promises and what skipping the fill could have broken.
out[#out+1] = "first=" .. E.l32(131072)
out[#out+1] = "last=" .. E.l32(196604)
E.s32(196604, 7)
out[#out+1] = "wrote=" .. E.l32(196604)
`
	want := "len_unchanged=true size=196608 first=0 last=0 wrote=7"
	sameAtEveryLevel(t, growWAT, growExpr(body), want)
}

// A PRE-BUILT WORD IS STILL OUT OF BOUNDS. The cursor is invisible because
// every path that can reach a word checks MEMSIZE first, and if that stopped
// being true the guest would be reading memory it was never given.
func TestPreBuiltWordsAreStillOutOfBounds(t *testing.T) {
	body := `
E.grow(1)
drain(4096)
out[#out+1] = "materialised=" .. shardlen(0)
out[#out+1] = "size_words=" .. (size() / 4)
local ok = pcall(E.l32, size())
out[#out+1] = "load_at_size_ok=" .. tostring(ok)
ok = pcall(E.s32, size(), 1)
out[#out+1] = "store_at_size_ok=" .. tostring(ok)
ok = pcall(E.l32, size() - 4)
out[#out+1] = "load_below_size_ok=" .. tostring(ok)
`
	want := "materialised=49152 size_words=32768 load_at_size_ok=false " +
		"store_at_size_ok=false load_below_size_ok=true"
	sameAtEveryLevel(t, growWAT, growExpr(body), want)
}

// A GUEST THAT NEVER GROWS PAYS NOTHING, and "nothing" is exact: not one word
// beyond its declared memory, and not one call to control.lua's hook.
//
// The chunk builds its declared memory through mem_grow with size = 0, so the
// guard that makes this true is one comparison -- and without it every guest in
// existence would carry a megabyte of lookahead it never asked for, in host
// RAM, in the save, and in Lua's own collector's walk.
func TestAGuestThatNeverGrowsPreBuildsNothing(t *testing.T) {
	body := `
local armed = 0
P.grow_hook(function() armed = armed + 1 end)
out[#out+1] = "armed=" .. armed
out[#out+1] = "materialised=" .. shardlen(0)
out[#out+1] = "size_words=" .. (size() / 4)
out[#out+1] = "owed=" .. tostring(P.prebuild(4096))
out[#out+1] = "still=" .. shardlen(0)
`
	want := "armed=0 materialised=16384 size_words=16384 owed=false still=16384"
	sameAtEveryLevel(t, growWAT, growExpr(body), want)
}

// THE LOOKAHEAD IS BOUNDED, AND THE BOUND IS THE POINT.
//
// A materialised word above MEMSIZE is a real Lua slot -- 16 B of host RAM,
// 2.29 B of save under --persist=table, and its share of the 0.202 ms/MiB Lua's
// own collector spends walking the memory. A lookahead of "one grow" would be
// unbounded for a guest whose growth law is a doubling, which is exactly
// TinyGo's, so it is capped at 1 MiB = 262,144 words.
//
// Sixteen pages is 262,144 words: a grow of that size asks for its own size
// again and gets exactly the cap. Thirty-two pages asks for twice the cap and
// gets the cap.
func TestTheLookaheadIsCappedAtAMegabyte(t *testing.T) {
	body := `
E.grow(16)
drain(65536)
out[#out+1] = "at16=" .. (shardlen(0) - size() / 4)
`
	// 17 pages is 278,528 words, which is inside shard 0 (524,288), and the
	// lookahead lands 262,144 past it -- so the cursor is at 540,672 and spills
	// into shard 1. shardlen(0) saturates at the shard, so the arithmetic is
	// done against the whole vector instead.
	body = `
E.grow(16)
drain(65536)
local total = shardlen(0) + shardlen(1)
out[#out+1] = "ahead16=" .. (total - size() / 4)
E.grow(32)
drain(65536)
total = shardlen(0) + shardlen(1) + shardlen(2)
out[#out+1] = "ahead32=" .. (total - size() / 4)
`
	want := "ahead16=262144 ahead32=262144"
	sameAtEveryLevel(t, growWAT, growExpr(body), want)
}

// ADOPT MOVES THE CURSOR, and a cursor left behind is the silent failure
// agents/sharding.md section 11 lists second, one level down: it claims words
// are materialised in a vector that has never seen them, so the next grow skips
// a fill it owed and hands the guest slots that are not there. A load of one is
// a nil, and nil arithmetic deep inside guest code is a long way from the grow
// that caused it.
//
// The shape is: grow, run the pre-build ahead, then adopt a SMALLER memory
// whose vector is exactly its own size, and grow again. Without the reset the
// second grow believes the words already exist.
func TestAdoptResetsTheFillCursor(t *testing.T) {
	body := `
E.grow(4)
drain(65536)
-- A fresh vector holding exactly one page of zeros, which is what a save of a
-- one-page guest restores.
local t0 = {}
for i = 1, 16384 do t0[i] = 0 end
P.adopt({ t0 }, 65536)
out[#out+1] = "adopted_size=" .. size()
out[#out+1] = "adopted_len=" .. shardlen(0)
E.grow(1)
out[#out+1] = "grown_len=" .. shardlen(0)
out[#out+1] = "new_word=" .. tostring(E.l32(65536))
out[#out+1] = "top_word=" .. tostring(E.l32(131068))
`
	want := "adopted_size=65536 adopted_len=16384 grown_len=32768 " +
		"new_word=0 top_word=0"
	sameAtEveryLevel(t, growWAT, growExpr(body), want)
}

// THE PRE-BUILD IS RESUMABLE AND ITS PIECES ARE ARBITRARY. control.lua hands it
// a fixed word budget per tick; nothing about correctness may depend on that
// number, and a piece boundary landing inside a shard, on a shard boundary, or
// past the target must all produce the same memory.
//
// One word at a time is the extreme, and it is run against a budget larger than
// the whole lookahead, because the two are the ends of the range every real
// budget sits inside.
func TestThePreBuildIsTheSameMemoryAtAnyPieceSize(t *testing.T) {
	for _, budget := range []int{1, 7, 4096, 524288, 1 << 20} {
		budget := budget
		t.Run(fmt.Sprintf("budget%d", budget), func(t *testing.T) {
			// One declared page plus two grows of two pages each: 65,536 ->
			// 196,608 -> 327,680 bytes. w0 is the first word the second grow
			// handed over and w1 the last, which are the two the pre-build
			// could have got wrong at a piece boundary.
			body := fmt.Sprintf(`
E.grow(2)
local n = 0
while P.prebuild(%d) do n = n + 1 if n > 400000 then error("stuck", 0) end end
out[#out+1] = "ahead=" .. (shardlen(0) - size() / 4)
E.grow(2)
out[#out+1] = "size=" .. size()
out[#out+1] = "w0=" .. E.l32(196608)
out[#out+1] = "w1=" .. E.l32(327676)
E.s32(327676, 9)
out[#out+1] = "wrote=" .. E.l32(327676)
`, budget)
			want := "ahead=32768 size=327680 w0=0 w1=0 wrote=9"
			got := runAt(t, growWAT, growExpr(body), analysis.O3)
			if got != want {
				t.Errorf("budget %d:\n got %s\nwant %s", budget, got, want)
			}
		})
	}
}
