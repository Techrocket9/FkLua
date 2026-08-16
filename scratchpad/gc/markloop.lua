-- The inner MARK and SWEEP loops, hand-written in the emitter's style.
--
-- THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE. It is the same instrument
-- bench/kernels/ is: hand-written Lua standing in for what the emitter would
-- produce, run under bin/lua52f. It exists to size N pages per tick before any
-- collector is built, and it is a CEILING in exactly the sense
-- agents/benchmarks.md means -- a real guest's mark loop arrives through TinyGo
-- and FkLua and will be slower, by the ~2x that file measures between the M0
-- kernels and a real guest.
--
-- It models a conservative mark over a word-table linear memory:
--   * every word in the scanned span is read out of MEM
--   * a word in [HS, HE) is treated as a pointer to a GRAN-byte granule
--   * the granule's mark bit is tested and set in a bitmap held as a second
--     Lua array (BITS), 32 bits per slot, arithmetic only -- no bit32, per
--     agents/codegen.md's "prefer arithmetic"
--   * a newly marked granule is pushed onto a gray work list, and `markdrain`
--     drains it the way an incremental step must
--
-- Written to the emitter's constraints on purpose, because that is what makes
-- the number transferable: no `local` after the prologue (Invariant B), i32
-- values as unsigned integral doubles (Invariant A), two-to-four scratch
-- registers rather than fresh names, and numeric `for` where a counted loop
-- would lower to one.
--
-- Usage:  lua52f markloop.lua <variant> <words> <reps> <ptr-density-percent>
--   variants: clear scan mark markdrain sweep
-- `clear` is the control: it does only the per-rep bitmap reset that scan/mark
-- also do, so the driver can subtract it rather than attributing it to marking.
-- Prints a checksum, so a variant that scans differently is caught.

local variant, wordsArg, repsArg, densArg = ...
local WORDS = tonumber(wordsArg) or 262144      -- 262144 words = 1 MiB
local REPS = tonumber(repsArg) or 8
local DENSITY = tonumber(densArg) or 20

local P2 = {}
do
  local v = 1
  for i = 0, 32 do P2[i] = v v = v * 2 end
end

-- The heap IS the linear memory, which is the whole point: [HS, HE) is a byte
-- range inside MEM. HS is above zero so that a small integer -- which is most
-- of what a Go heap holds -- is not mistaken for a pointer, and above it is
-- where conservative false retention comes from.
local MEM = {}
local BYTES = WORDS * 4
local HS = 65536
local HE = BYTES
local GRAN = 16

-- MINSTD rather than a 32-bit LCG on purpose: 2^31 * 16807 is 3.6e13, inside a
-- double's 9.0e15 exact-integer ceiling, where the usual `s * 1103515245`
-- silently loses bits under Invariant A's representation -- the same overflow
-- agents/benchmarks.md records the Lua FNV kernel hitting.
do
  local s = 1
  for i = 1, WORDS do
    s = (s * 16807) % 2147483647
    if (s % 100) < DENSITY then
      MEM[i] = HS + (s % (HE - HS))
    else
      MEM[i] = s % 65536
    end
  end
end

local NGRAN = (HE - HS) / GRAN
local NSLOT = NGRAN - NGRAN % 32
NSLOT = NSLOT / 32 + 1
local BITS = {}
local GRAY = {}
local GN = 0

-- Scratch registers, declared once, exactly as a generated function declares
-- them.
local t0, t1, t2, t3 = 0, 0, 0, 0
local acc = 0

local function clear()
  for i = 1, NSLOT do BITS[i] = 0 end
end

-- The range test only: a table read and two compares per word, which is what a
-- NON-pointer word costs -- and most words are non-pointers, so this is the
-- floor the whole design sits on.
local function scan()
  for i = 1, WORDS do
    t0 = MEM[i]
    if t0 >= HS and t0 < HE then acc = acc + 1 end
  end
end

-- Range test, granule index, bitmap test-and-set, gray push.
local function mark()
  for i = 1, WORDS do
    t0 = MEM[i]
    if t0 >= HS and t0 < HE then
      t0 = t0 - HS
      t0 = (t0 - t0 % GRAN) / GRAN
      t1 = (t0 - t0 % 32) / 32
      t2 = P2[t0 % 32]
      t3 = BITS[t1 + 1]
      if t3 % (t2 + t2) < t2 then
        BITS[t1 + 1] = t3 + t2
        GN = GN + 1
        GRAY[GN] = t0
        acc = acc + 1
      end
    end
  end
end

-- mark, plus draining the gray list: every granule that got marked has its own
-- GRAN bytes scanned in turn, which is the transitive half of the closure and
-- the part a step budget actually has to bound.
local function markdrain()
  mark()
  t3 = 1
  while t3 <= GN do
    t0 = (HS + GRAY[t3] * GRAN) / 4 + 1
    for k = 0, GRAN / 4 - 1 do
      t1 = MEM[t0 + k]
      if t1 ~= nil and t1 >= HS and t1 < HE then acc = acc + 1 end
    end
    t3 = t3 + 1
    if t3 > 65536 then break end
  end
end

-- Walk the mark bitmap and thread unmarked granules onto a free list. A slot
-- that is all ones is skipped whole, which is the case worth having and the
-- reason to hold marks as bits rather than bytes.
local FREE = {}
local FN = 0
local function sweep()
  for i = 1, NSLOT do
    t0 = BITS[i]
    if t0 ~= 4294967295 then
      for k = 0, 31 do
        t1 = P2[k]
        if t0 % (t1 + t1) < t1 then
          FN = FN + 1
          FREE[FN] = (i - 1) * 32 + k
          acc = acc + 1
        end
      end
    end
    BITS[i] = 0
  end
end

clear()
if variant == "sweep" then
  -- Sweeping a bitmap the mark phase filled in, so the all-ones fast path is
  -- exercised at a realistic rate rather than never.
  mark()
  for r = 1, REPS do FN = 0 sweep() mark() end
else
  local f = { clear = function() end, scan = scan, mark = mark, markdrain = markdrain }
  local g = f[variant]
  if g == nil then error("unknown variant " .. tostring(variant)) end
  for r = 1, REPS do
    clear()
    GN = 0
    g()
  end
end
print(string.format("%d", acc % 4294967296), NSLOT, NGRAN)
