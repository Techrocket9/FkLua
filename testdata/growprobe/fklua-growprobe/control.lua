-- FkLua memory.grow cost probe.
--
-- WHY A BARE LUA MOD, AND WHY IN THE GAME. The whole quantity being measured is
-- what it costs Factorio's Lua to CREATE a table slot, and `bin/lua52f` is the
-- wrong instrument for that by the same rule agents/sharding.md opens with: the
-- oracle's array part grows to 2^30 and Factorio's stops at 2^20, and even below
-- any wall the oracle's table read is 4-6x faster (agents/sharding.md section 2).
-- A bare mod with no wasm, no emitter, no --persist and no collector in it is
-- what makes these numbers attributable to the REPRESENTATION rather than to
-- FkLua, which is how the wall itself was established.
--
-- WHAT IS MEASURED. `grow` below is `mem_grow`'s fill loop copied verbatim out
-- of runtime/lua/fk_rt.lua -- shard by shard, ascending, creating each shard
-- empty and filling it sequentially. Everything else in mem_grow is two
-- comparisons and a division, so the fill IS the function.
--
--   build_<inc>   grow from 0 to TOP by <inc> words at a time, timing the WHOLE
--                 build. This is the TOTAL cost of an increment policy, and it
--                 is what says whether smaller increments cost more in total.
--   grow_<inc>@S  ONE grow of <inc> words with the memory already at S. This is
--                 the WORST TICK an increment policy leaves behind.
--   pre_<inc>@S   the same words materialised AHEAD of MEMSIZE (the pre-build
--                 candidate), then "grown into" by bumping the size. Two
--                 numbers: what the pre-build costs and what the splice costs.
--   fill_<inc>@S  mem_fill over slots that ALREADY EXIST, for the same span.
--                 The difference between this and grow is the cost of CREATING
--                 a slot as opposed to writing one, which is the whole subject.
--
-- DISCIPLINE (agents/benchmarks.md). Every timed arm reports the word count
-- beside it so the analysis divides rather than guesses, and each size band is
-- measured on a freshly built memory so no arm inherits another's table shape.
-- Run-to-run spread in this game is large (agents/sharding.md: up to 31% on an
-- access microbenchmark), so RUNS>1 and the analysis reports a range.

local SHW = 524288           -- 2^19 words per shard, exactly half the 2^20 wall
local HALF = 262144          -- 2^18 -- where the LAST array-part doubling starts
local PIECE = 8192           -- PREBUILD_BUDGET in runtime/lua/fk_mod.lua, verbatim

local sink = 0

-- Timing goes through the log because helpers.create_profiler() is the only
-- clock in the sandbox and it refuses to hand Lua a raw number.
local function timed(label, words, fn)
  local p = helpers.create_profiler()
  local r = fn()
  p.stop()
  sink = sink + (r or 0)
  log({ "", "FKGROW\t" .. label .. "\t" .. words .. "\t", p })
end

-- ---------------------------------------------------------------------------
-- mem_grow's fill, verbatim from runtime/lua/fk_rt.lua.
--
-- Word indices rather than bytes, because everything here is a word count and
-- the byte/4 in the runtime is not part of what is being measured.
-- ---------------------------------------------------------------------------
local function grow(mem, from, to)
  local w = from
  while w < to do
    local off = w % SHW
    local s = (w - off) / SHW + 1
    local t = mem[s]
    if not t then t = {} mem[s] = t end
    local k = SHW - off
    if k > to - w then k = to - w end
    for i = off + 1, off + k do t[i] = 0 end
    w = w + k
  end
end

-- A store into slots that already exist, over the same span. mem_fill's inner
-- loop, with the same shard walk.
local function fillexisting(mem, from, to)
  local w = from
  while w < to do
    local off = w % SHW
    local s = (w - off) / SHW + 1
    local t = mem[s]
    local k = SHW - off
    if k > to - w then k = to - w end
    for i = off + 1, off + k do t[i] = 0 end
    w = w + k
  end
end

-- The SPLICE: what a grow costs when the words it wants are already
-- materialised. This is the whole of mem_grow under a fill cursor that kept up.
--
-- IT DOES NOT RE-ZERO THEM, and that is the design rather than a shortcut: a
-- word between MEMSIZE and the fill cursor is above the bounds check every
-- emitted access, mem_copy, mem_fill and st8raw opens with, so nothing in the
-- guest or the host can have written it since it was created. The pre-built
-- words are still zero by construction, so the grow's whole job is to move the
-- cursor. What is left is one comparison and the SHBOUND derivation.
local function splice(mem, from, to, fill)
  local w = from
  if w < fill then w = fill end
  while w < to do
    local off = w % SHW
    local s = (w - off) / SHW + 1
    local t = mem[s]
    if not t then t = {} mem[s] = t end
    local k = SHW - off
    if k > to - w then k = to - w end
    for i = off + 1, off + k do t[i] = 0 end
    w = w + k
  end
  local sb = to * 4
  if sb > 2097152 then sb = 2097152 end
  return sb
end

-- THE CLONE CANDIDATE. `{table.unpack(z, 1, SHW)}` is the ONE constructor form
-- in Lua 5.2 that presizes an array part, and a shard is 524,288 words -- under
-- LUAI_MAXSTACK, where a whole flat memory is not, which is why
-- runtime/lua/fk_rt.lua's comment says a presize is impossible and is right
-- about the FLAT table and possibly wrong about a SHARD. If a zero shard can be
-- cloned in C for less than the 56 ms its fill costs, the pacing question
-- changes shape entirely.
--
-- It is measured rather than assumed for two reasons beyond speed: it needs
-- SHW stack slots at once, and it is one indivisible C call, so it cannot be
-- paced even if it is fast.
local function clone_shard(z)
  return { table.unpack(z, 1, SHW) }
end

-- The same clone at an arbitrary width, for the hybrid arm in section 6. A
-- presize to HALF a shard is the shape that would be worth having IF Lua's
-- array-part doublings landed such that the last one were thereby skipped;
-- whether they do is a fact about ltable.c's rehash and is measured rather
-- than reasoned, because this project has now been wrong twice about a Lua
-- table internal it read rather than timed.
local function clone_n(z, n)
  return { table.unpack(z, 1, n) }
end

-- Read one word back so an arm cannot be optimised into nothing and so a
-- mis-built memory shows up as an error rather than as a fast number.
local function probe(mem, w)
  local off = w % SHW
  local t = mem[(w - off) / SHW + 1]
  if t == nil then error("probe: shard missing at word " .. w, 0) end
  local v = t[off + 1]
  if v == nil then error("probe: word " .. w .. " was never materialised", 0) end
  return v
end

-- ---------------------------------------------------------------------------
-- The arms.
-- ---------------------------------------------------------------------------

-- TOP is where every build stops. 40 MiB is the size agents/guests.md's grow
-- table reports a 288-365 ms tick at, and it is the size the past-the-cap
-- gcbench arm actually reaches.
local TOP = 40 * 262144      -- 40 MiB in words = 10,485,760

-- The increments, in words. 16,384 words is 64 KiB, which is ONE WASM PAGE and
-- therefore the smallest grow the ABI can express -- the floor of any
-- bounded-increment policy.
local INCS = {
  { name = "64KiB", w = 16384 },
  { name = "256KiB", w = 65536 },
  { name = "1MiB", w = 262144 },
  { name = "4MiB", w = 1048576 },
}

-- The sizes a single grow is measured AT. 4 MiB is a small guest, 16 MiB is
-- where the old fkgc cap was, 40 MiB is past it.
local ATS = { 4, 16, 40 }

local function fresh()
  collectgarbage("collect")
  return {}
end

local function build_to(mem, words)
  grow(mem, 0, words)
end

local function run()
  log("FKGROW_BEGIN")

  -- 1. THE TOTAL COST OF AN INCREMENT POLICY. Same 40 MiB reached four ways.
  --    Every word is written exactly once in every arm, so any difference is
  --    the per-CALL cost multiplied by the call count -- which is the trade the
  --    storm test already asserts and this row prices.
  for _, inc in ipairs(INCS) do
    local mem = fresh()
    local n = inc.w
    timed("build_" .. inc.name, TOP, function()
      local w = 0
      while w < TOP do
        local to = w + n
        if to > TOP then to = TOP end
        grow(mem, w, to)
        w = to
      end
      return probe(mem, TOP - 1)
    end)
    mem = nil
  end

  -- 2. WHAT ONE GROW COSTS, at three heap sizes and four increments. This is
  --    the worst tick each policy leaves, and it is the number the whole
  --    milestone is about.
  for _, at in ipairs(ATS) do
    local base = at * 262144
    for _, inc in ipairs(INCS) do
      local mem = fresh()
      build_to(mem, base)
      timed("grow_" .. inc.name .. "@" .. at, inc.w, function()
        grow(mem, base, base + inc.w)
        return probe(mem, base + inc.w - 1)
      end)
      -- The same span written into slots that already exist. The difference is
      -- the cost of CREATING a slot rather than writing one.
      timed("fill_" .. inc.name .. "@" .. at, inc.w, function()
        fillexisting(mem, base, base + inc.w)
        return probe(mem, base + inc.w - 1)
      end)
      mem = nil
    end
  end

  -- 3. THE PRE-BUILD CANDIDATE. The words are materialised ahead of the size
  --    the guest can see, and the grow that follows finds them already there.
  --    Two numbers per row: the pre-build (which is paced and lands on some
  --    OTHER tick) and the splice (which is what the growing tick actually
  --    pays).
  for _, at in ipairs(ATS) do
    local base = at * 262144
    for _, inc in ipairs(INCS) do
      local mem = fresh()
      build_to(mem, base)
      timed("pre_" .. inc.name .. "@" .. at, inc.w, function()
        grow(mem, base, base + inc.w)
        return probe(mem, base + inc.w - 1)
      end)
      timed("splice_" .. inc.name .. "@" .. at, inc.w, function()
        return splice(mem, base, base + inc.w, base + inc.w)
      end)
      mem = nil
    end
  end

  -- 4. WHAT A PACED PRE-BUILD STEP COSTS. The pre-build has to be cut into
  --    bounded pieces or it is the same stall on a different tick, so the piece
  --    machinery is timed at the size a piece would actually be: one piece is
  --    charged, the whole shard is walked in pieces, and the total is compared
  --    with the same shard built in one go. Any gap is the per-piece overhead
  --    the pacing costs.
  for _, at in ipairs(ATS) do
    local base = at * 262144
    local mem = fresh()
    build_to(mem, base)
    timed("oneshot_shard@" .. at, SHW, function()
      grow(mem, base, base + SHW)
      return probe(mem, base + SHW - 1)
    end)
    mem = nil

    mem = fresh()
    build_to(mem, base)
    -- 8,192 words is ~0.9 ms at the rate this probe is measuring, which is a
    -- pacing granule in the same neighbourhood as the collector's 1,024-granule
    -- budget. It is the piece size, not a tuned answer.
    timed("paced_shard@" .. at, SHW, function()
      local w = base
      local last = base + SHW
      while w < last do
        local to = w + 8192
        if to > last then to = last end
        grow(mem, w, to)
        w = to
      end
      return probe(mem, base + SHW - 1)
    end)
    mem = nil
  end

  -- 5. THE CLONE. One zero shard built the ordinary way, then cloned. If this
  --    is cheap, a whole-shard grow is a C memcpy rather than half a million
  --    VM iterations, and the pacing question is a different one.
  --
  -- grow() builds a shard VECTOR, so the template is its shard 0 and not the
  -- vector. (The first version of this arm cloned the vector and reported
  -- `got 1 words`, which is the assertion doing its job.)
  --
  -- THE TEMPLATE IS ITSELF A COST AND IT IS NOT TIMED HERE. A permanently live
  -- 2^19-word zero shard is 8 MiB of host RAM, 2.29 B/word of save under
  -- --persist=table, and one more table for Lua's own collector to walk -- one
  -- extra ~0.5 ms propagatemark per cycle, forever, on every guest whether or
  -- not it ever grows. Section 6 weighs that against what it buys.
  local zv = {}
  grow(zv, 0, SHW)
  local z = zv[1]
  do
    for _, at in ipairs(ATS) do
      local base = at * 262144
      local mem = fresh()
      build_to(mem, base)
      timed("clone_shard@" .. at, SHW, function()
        local ok, t = pcall(clone_shard, z)
        if not ok then
          log("FKGROW_CLONE_REFUSED " .. tostring(t))
          return 0
        end
        if #t ~= SHW then error("clone: got " .. #t .. " words", 0) end
        if t[1] ~= 0 or t[SHW] ~= 0 then error("clone: not zeroed", 0) end
        mem[base / 524288 + 1] = t
        return t[SHW]
      end)
      mem = nil
    end
  end

  -- 6. THE SHARD-DOUBLING RESIDUAL: three shapes for the same 2 MiB shard.
  --
  -- What is left of a grow tick after sharding stage 15's pacing is ONE
  -- array-part doubling per shard ever filled -- 2^18 -> 2^19 entries,
  -- 16.2-19.1 ms, at every odd megabyte, indivisible. Section 15 recorded the
  -- presize as REFUSED on the grounds that it is one C call that cannot be
  -- paced. That refusal was taken BEFORE the fill was paced, so the comparison
  -- it made ("40% off a 57 ms stall") is no longer the comparison in front of
  -- anyone. This section retakes it against the shape that actually ships.
  --
  --   A  pace-plus-doubling, WHAT SHIPS: 64 pieces of PREBUILD_BUDGET words,
  --      exactly one of which carries the doubling.
  --   B  presize-at-creation: the shard is CLONED from a zero template, which
  --      sizes the array part to 2^19 in one C call and leaves NOTHING to
  --      fill. One indivisible piece and no others.
  --   C  the hybrid: presize to 2^18 -- HALF -- and pace the rest, on the
  --      theory that the last doubling is thereby skipped rather than merely
  --      moved. Where Lua's doublings land decides that, and this arm decides
  --      it by measurement.
  --
  -- THE NUMBER THAT CHOOSES IS THE WORST PIECE, NOT THE TOTAL. The total lands
  -- on ticks nobody is waiting on -- that is what pacing did -- so a shape that
  -- is cheaper in aggregate and larger in its largest piece is a REGRESSION
  -- here, which is the opposite of how section 15's original refusal was
  -- scored.
  --
  -- EVERY PIECE IS LOGGED SEPARATELY, because helpers.create_profiler() will
  -- not hand Lua a raw number and a maximum therefore cannot be computed in
  -- here. The analysis takes the max over an arm's pieces, which is what a
  -- worst tick IS. Each arm is also given a freshly built memory, so no arm
  -- inherits another's table shape -- B in particular installs a shard the
  -- ordinary path never built.
  for _, at in ipairs(ATS) do
    local base = at * 262144
    local si = base / SHW + 1
    local pieces = SHW / PIECE

    -- A -- the shipped shape. One of these 64 pieces is the doubling.
    local mem = fresh()
    build_to(mem, base)
    for i = 0, pieces - 1 do
      local w = base + i * PIECE
      timed("Apiece@" .. at .. "#" .. i, PIECE, function()
        grow(mem, w, w + PIECE)
        return probe(mem, w + PIECE - 1)
      end)
    end
    mem = nil

    -- B -- presize at creation. The clone is the whole cost; the pieces after
    -- it are what the pre-build would still be asked to do, measured rather
    -- than assumed to be free.
    mem = fresh()
    build_to(mem, base)
    timed("Bpresize@" .. at, SHW, function()
      local ok, t = pcall(clone_n, z, SHW)
      if not ok then log("FKGROW_CLONE_REFUSED " .. tostring(t)) return 0 end
      if #t ~= SHW then error("B: clone gave " .. #t .. " words", 0) end
      if t[1] ~= 0 or t[SHW] ~= 0 then error("B: clone not zeroed", 0) end
      mem[si] = t
      return t[SHW]
    end)
    for i = 0, pieces - 1 do
      local w = base + i * PIECE
      timed("Bpiece@" .. at .. "#" .. i, PIECE, function()
        grow(mem, w, w + PIECE)
        return probe(mem, w + PIECE - 1)
      end)
    end
    mem = nil

    -- C -- presize HALF, pace the rest. The pieces start at HALF because the
    -- clone already materialised everything below it.
    mem = fresh()
    build_to(mem, base)
    timed("Cpresize@" .. at, HALF, function()
      local ok, t = pcall(clone_n, z, HALF)
      if not ok then log("FKGROW_CLONE_REFUSED " .. tostring(t)) return 0 end
      if #t ~= HALF then error("C: clone gave " .. #t .. " words", 0) end
      if t[1] ~= 0 or t[HALF] ~= 0 then error("C: clone not zeroed", 0) end
      mem[si] = t
      return t[HALF]
    end)
    for i = HALF / PIECE, pieces - 1 do
      local w = base + i * PIECE
      timed("Cpiece@" .. at .. "#" .. i, PIECE, function()
        grow(mem, w, w + PIECE)
        return probe(mem, w + PIECE - 1)
      end)
    end
    mem = nil
  end

  log("FKGROW_SINK " .. sink)
  log("FKGROW_END")
end

local done = false
script.on_event(defines.events.on_tick, function()
  if done then return end
  done = true
  run()
end)
