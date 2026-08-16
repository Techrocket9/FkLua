-- FkLua sharded-linear-memory probe.
--
-- WHY A BARE LUA MOD. The 4 MiB wall (agents/sandbox.md, agents/gc.md "The
-- 4 MiB wall") is a fact about Factorio's Lua TABLE INTERNALS, and
-- `bin/lua52f` -- stock 5.2.1, array part to 2^30 -- cannot see it in either
-- direction. Every number about the wall has to come from the game. Doing it
-- with no wasm, no emitter, no --persist and no collector in the mod is what
-- established the wall in the first place, and it is what makes these numbers
-- attributable to the representation rather than to FkLua.
--
-- WHAT IS MEASURED. Each variant is the ACTUAL EMITTED SHAPE of an -opt=3
-- i32 access, copied from internal/luagen/luagen.go's emitInlineStore32 /
-- emitInlineLoad32 and internal/luagen/loopguard.go's guarded arm, with the
-- bounds check, the alignment test and the MEMDIRTY test all present -- because
-- the shard arithmetic's cost is only meaningful against a baseline that
-- carries everything else the real access carries.
--
--   flat        MEM[t0 / 4 + 1]                       -- today
--   divmod      MEMS[(t0 - t0 % SHB) / SHB + 1][...]  -- shard select, arithmetic
--   bit32       MEMS[rshift(t0,21)+1][band(...)+1]     -- shard select, bit32
--   basekey     SH[t0 - t0 % SHB][...]                -- shard select, hash lookup
--   guarded     ls[lw + k]                            -- shard hoisted to a local
--   dispatch_b  if SHARDED then <divmod> else <flat> end
--   dispatch_f  LD(t0)                                 -- upvalue function swap
--
-- Timing goes through the log because helpers.create_profiler() is the only
-- clock in the sandbox and refuses to hand Lua a raw number.
--
-- DISCIPLINE (agents/benchmarks.md): every arm is bracketed by an A/A of the
-- flat arm measured in the same run at the same memory size, so a ratio quoted
-- from here has a floor measured beside it rather than a bare wall-clock
-- figure.

local MEM                    -- the flat word table: today's representation
local MEMS                   -- shards, MEMS[s+1] is words [s*SHW, (s+1)*SHW)
local SH                     -- shards keyed by SHARD BASE BYTE ADDRESS
local MEMSIZE = 0            -- bytes
local MEMDIRTY = false       -- packed/collector arm; false here, as table mode
local DPLO, DPHI = math.huge, -1
local SHARDED = false        -- the runtime-transition mode flag (candidate b)
local LD, ST                 -- the upvalue function slots (candidate b)

-- 2^19 words = 2 MiB per shard. Every shard is half the 2^20 key wall, so a
-- shard can never stop being an array however the memory grows.
local SHW = 524288
local SHB = SHW * 4          -- 2097152 bytes

-- S1 is shard 0 bound to a chunk-local, exactly the status MEM has today, and
-- SHBOUND is min(MEMSIZE, SHB) -- the number of bytes reachable through S1.
--
-- These two are the whole of the FOURTH candidate, which neither the design
-- sketch nor the first two probe runs contained: fold the shard test INTO the
-- bounds check. Every emitted access already opens with
-- `if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end`, and below 2 MiB
-- SHBOUND *is* MEMSIZE -- so `t0 + 4 <= SHBOUND` is the bounds check, not an
-- addition to it, and the fast arm is today's access unchanged. Above 2 MiB
-- the same test decides shard 0 versus everything else.
local S1
local SHBOUND = 0

local rshift, band = bit32.rshift, bit32.band

local sink = 0
local REPS = 400000          -- accesses per timed arm

local function trap_oob() error("oob", 0) end

-- ---------------------------------------------------------------------------
-- Timing
-- ---------------------------------------------------------------------------

local function timed(label, reps, fn)
  local p = helpers.create_profiler()
  local r = fn()
  p.stop()
  sink = sink + (r or 0)
  log({ "", "FKSHARD\t" .. label .. "\t" .. reps .. "\t", p })
end

-- ---------------------------------------------------------------------------
-- Construction. This is also the LOAD-TIME measurement: control.lua rebuilds
-- the word table from `storage` on every load, so "build N words from empty" IS
-- the per-load cost agents/gc.md priced at 2.9 s / 5.3 s.
-- ---------------------------------------------------------------------------

local function build_flat(words)
  local m = {}
  for i = 1, words do m[i] = i % 251 end
  return m
end

local function build_shards(words)
  local out, keyed = {}, {}
  local n = math.ceil(words / SHW)
  for s = 0, n - 1 do
    local lo = s * SHW
    local hi = lo + SHW
    if hi > words then hi = words end
    local t = {}
    for i = 1, hi - lo do t[i] = (lo + i) % 251 end
    out[s + 1] = t
    keyed[s * SHB] = t
  end
  return out, keyed
end

-- ---------------------------------------------------------------------------
-- LOAD arms. Each is the emitted -opt=3 inline i32 load, whole.
-- ---------------------------------------------------------------------------

local function ld_flat(n)
  local s, t0 = 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then s = s + MEM[t0 / 4 + 1] else s = s + 1 end
  end
  return s
end

-- ld_flat with the TABLE READ REMOVED and nothing else changed. It is the
-- floor for every access number here, and it is also the arm that settles
-- agents/gc.md's stage-D open item 1: the same two loops run under bin/lua52f
-- give a host-side figure for an interpreter with no wall in it, so the
-- Factorio/oracle ratio on THIS pair is a property of the interpreter rather
-- than of the table. Nothing else in this repo has ever measured that ratio,
-- and several published numbers depend on it being 1.
local function ld_ctl(n)
  local s, t0 = 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then s = s + 1 else s = s + 1 end
  end
  return s
end

local function ld_divmod(n)
  local s, t0, t1 = 0, 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then
      t1 = t0 % SHB
      s = s + MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1]
    else s = s + 1 end
  end
  return s
end

local function ld_bit32(n)
  local s, t0 = 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then
      s = s + MEMS[rshift(t0, 21) + 1][band(rshift(t0, 2), SHW - 1) + 1]
    else s = s + 1 end
  end
  return s
end

local function ld_basekey(n)
  local s, t0, t1 = 0, 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then
      t1 = t0 % SHB
      s = s + SH[t0 - t1][t1 / 4 + 1]
    else s = s + 1 end
  end
  return s
end

-- The GUARDED arm, both representations. This is the one that decides
-- candidate (a): a loop whose span the guard proved within one shard binds the
-- shard table to a LOCAL and steps a shard-local word index, so the access is
-- one table read off a local -- which is what today's guarded access already
-- is, except that today's table is the chunk-local MEM (an upvalue).
local function ld_guard_flat(n)
  local s, lw = 0, 1
  local lm = MEM
  for _ = 1, n do
    s = s + lm[lw]
    lw = lw + 1
  end
  return s
end

local function ld_guard_shard(n)
  local s, lw = 0, 1
  local ls = MEMS[1]
  for _ = 1, n do
    s = s + ls[lw]
    lw = lw + 1
  end
  return s
end

-- The guarded arm as it exists TODAY in emitted code: MEM is an upvalue, not a
-- function local. Kept separate so the local-vs-upvalue difference is visible
-- rather than folded into the sharding delta.
local function ld_guard_upval(n)
  local s, lw = 0, 1
  for _ = 1, n do
    s = s + MEM[lw]
    lw = lw + 1
  end
  return s
end

-- CANDIDATE (d): the bounds check IS the shard test.
--
-- The whole access, as the emitter would print it. Note what is NOT here: the
-- separate `if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end` line, because
-- the fast arm's guard subsumes it. Below 2 MiB, SHBOUND == MEMSIZE and this
-- is today's access with its two `if`s merged into one; there is no arm to
-- take but the first, ever. Above 2 MiB, the else arm handles both the higher
-- shards and every trap.
local function ld_s0fast(n)
  local s, t0, t1 = 0, 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then
      s = s + S1[t0 / 4 + 1]
    else
      if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
      t1 = t0 % SHB
      s = s + MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1]
    end
  end
  return s
end

local function st_s0fast(n)
  local t0, t1 = 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then DPLO = t0 end
    if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then
      S1[t0 / 4 + 1] = a % 4294967296.0
    else
      if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
      t1 = t0 % SHB
      MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1] = a % 4294967296.0
    end
  end
  return 0
end

-- The same, walking the WHOLE memory, so above the wall the fast arm misses on
-- every address outside shard 0. This is the arm that says what candidate (d)
-- costs a guest whose working set is not at the bottom of its memory.
local function ld_s0fast_spread(n)
  local s, t0, t1, a = 0, 0, 0, 0
  for _ = 1, n do
    a = a + 8192
    if a + 4 > MEMSIZE then a = a - MEMSIZE + 4 end
    t0 = a
    if t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0 then
      s = s + S1[t0 / 4 + 1]
    else
      if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
      t1 = t0 % SHB
      s = s + MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1]
    end
  end
  return s
end

-- Candidate (b): mode dispatch.
local function ld_dispatch_branch(n)
  local s, t0, t1 = 0, 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then
      if SHARDED then
        t1 = t0 % SHB
        s = s + MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1]
      else
        s = s + MEM[t0 / 4 + 1]
      end
    else s = s + 1 end
  end
  return s
end

local function ld_dispatch_fn(n)
  local s, t0 = 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    s = s + LD(t0)
  end
  return s
end

-- ---------------------------------------------------------------------------
-- STORE arms. emitInlineStore32's body verbatim, plus the shard select.
-- ---------------------------------------------------------------------------

local function st_flat(n)
  local t0 = 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then DPLO = t0 end
    if t0 % 4 == 0 then MEM[t0 / 4 + 1] = a % 4294967296.0 end
  end
  return 0
end

local function st_divmod(n)
  local t0, t1 = 0, 0
  for a = 0, (n - 1) * 4, 4 do
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then DPLO = t0 end
    if t0 % 4 == 0 then
      t1 = t0 % SHB
      MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1] = a % 4294967296.0
    end
  end
  return 0
end

local function st_guard_flat(n)
  local lw = 1
  local lm = MEM
  for i = 1, n do
    lm[lw] = i % 4294967296.0
    lw = lw + 1
  end
  return 0
end

local function st_guard_shard(n)
  local lw = 1
  local ls = MEMS[1]
  for i = 1, n do
    ls[lw] = i % 4294967296.0
    lw = lw + 1
  end
  return 0
end

-- ---------------------------------------------------------------------------
-- SPREAD address pattern. The arms above walk addresses 0..1.6 MB, which is
-- the wall probe's own design -- the same indices at every table size, so the
-- only thing that changes is the representation. But sequential-from-zero
-- touches ONE shard, which flatters the sharded arm's locality. These walk the
-- WHOLE memory with a 8 KiB stride so every shard is live.
-- ---------------------------------------------------------------------------

local function ld_flat_spread(n)
  local s, t0, a = 0, 0, 0
  for _ = 1, n do
    a = a + 8192
    if a + 4 > MEMSIZE then a = a - MEMSIZE + 4 end
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then s = s + MEM[t0 / 4 + 1] else s = s + 1 end
  end
  return s
end

local function ld_divmod_spread(n)
  local s, t0, t1, a = 0, 0, 0, 0
  for _ = 1, n do
    a = a + 8192
    if a + 4 > MEMSIZE then a = a - MEMSIZE + 4 end
    t0 = a
    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
    if t0 % 4 == 0 then
      t1 = t0 % SHB
      s = s + MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1]
    else s = s + 1 end
  end
  return s
end

-- ---------------------------------------------------------------------------
-- Bulk: mem_fill's inner loop, within one shard and across a boundary.
-- fk_rt.lua's mem_fill writes the word table directly in a plain numeric for.
-- ---------------------------------------------------------------------------

local function fill_flat(words, from)
  local m = MEM
  for i = from + 1, from + words do m[i] = 0 end
  return 0
end

-- The crossing form: split the span at every shard boundary and run one plain
-- loop per piece. This is the design answer for mem_copy/mem_fill/fk_wstr.
local function fill_shard_split(words, from)
  local w0, w1 = from, from + words
  while w0 < w1 do
    local s = (w0 - w0 % SHW) / SHW
    local lo = w0 % SHW
    local hi = w1 - s * SHW
    if hi > SHW then hi = SHW end
    local t = MEMS[s + 1]
    for i = lo + 1, hi do t[i] = 0 end
    w0 = w0 + (hi - lo)
  end
  return 0
end

-- ---------------------------------------------------------------------------
-- The run
-- ---------------------------------------------------------------------------

local function run_size(name, words)
  log("FKSHARD_SIZE\t" .. name .. "\t" .. words)

  timed("build_flat/" .. name, words, function()
    MEM = build_flat(words)
    return 0
  end)
  timed("build_shard/" .. name, words, function()
    MEMS, SH = build_shards(words)
    return 0
  end)

  MEMSIZE = words * 4
  local n = REPS
  if n * 4 > MEMSIZE then n = math.floor(MEMSIZE / 4) end

  -- A/A floor: the flat load, first and last, bracketing everything.
  timed("aa_head/" .. name, n, function() return ld_flat(n) end)

  timed("ld_ctl/" .. name, n, function() return ld_ctl(n) end)
  timed("ld_flat/" .. name, n, function() return ld_flat(n) end)
  timed("ld_divmod/" .. name, n, function() return ld_divmod(n) end)
  timed("ld_bit32/" .. name, n, function() return ld_bit32(n) end)
  timed("ld_basekey/" .. name, n, function() return ld_basekey(n) end)
  timed("ld_guard_upval/" .. name, n, function() return ld_guard_upval(n) end)
  timed("ld_guard_flat/" .. name, n, function() return ld_guard_flat(n) end)
  timed("ld_guard_shard/" .. name, n, function() return ld_guard_shard(n) end)

  SHARDED = false
  timed("ld_dispatch_branch_flat/" .. name, n, function() return ld_dispatch_branch(n) end)
  SHARDED = true
  timed("ld_dispatch_branch_shard/" .. name, n, function() return ld_dispatch_branch(n) end)
  SHARDED = false

  LD = function(t0) return MEM[t0 / 4 + 1] end
  timed("ld_dispatch_fn_flat/" .. name, n, function() return ld_dispatch_fn(n) end)
  LD = function(t0)
    local t1 = t0 % SHB
    return MEMS[(t0 - t1) / SHB + 1][t1 / 4 + 1]
  end
  timed("ld_dispatch_fn_shard/" .. name, n, function() return ld_dispatch_fn(n) end)

  timed("st_flat/" .. name, n, function() return st_flat(n) end)
  timed("st_divmod/" .. name, n, function() return st_divmod(n) end)
  timed("st_guard_flat/" .. name, n, function() return st_guard_flat(n) end)
  timed("st_guard_shard/" .. name, n, function() return st_guard_shard(n) end)

  timed("ld_flat_spread/" .. name, n, function() return ld_flat_spread(n) end)
  timed("ld_divmod_spread/" .. name, n, function() return ld_divmod_spread(n) end)

  -- Bulk, 256 Ki words = 1 MiB, deliberately straddling a shard boundary in
  -- the second arm (start 64 Ki words before the boundary).
  local bulk = 262144
  local from = 0
  if words > SHW + bulk then from = SHW - 65536 end
  timed("fill_flat/" .. name, bulk, function() return fill_flat(bulk, from) end)
  timed("fill_shard_split/" .. name, bulk, function() return fill_shard_split(bulk, from) end)

  timed("aa_tail/" .. name, n, function() return ld_flat(n) end)

  -- Release before the next size so peak host memory stays sane.
  MEM, MEMS, SH = nil, nil, nil
  collectgarbage("collect")
end

-- The CURVE. run_size above measures everything at four sizes; this measures
-- the few arms that decide WHERE the flat representation turns, at enough
-- sizes to see it, and it measures the flat arms BEFORE the shards exist so
-- that "a bigger flat table" is not confounded with "more live host memory".
local function run_curve(name, words)
  log("FKSHARD_SIZE\tcurve/" .. name .. "\t" .. words)
  MEM = build_flat(words)
  MEMSIZE = words * 4
  local n = REPS
  if n * 4 > MEMSIZE then n = math.floor(MEMSIZE / 4) end

  -- Flat, with NO shards allocated yet.
  timed("c_ctl/" .. name, n, function() return ld_ctl(n) end)
  timed("c_ld_flat/" .. name, n, function() return ld_flat(n) end)
  timed("c_st_flat/" .. name, n, function() return st_flat(n) end)

  timed("c_build_shard/" .. name, words, function()
    MEMS, SH = build_shards(words)
    S1 = MEMS[1]
    SHBOUND = MEMSIZE < SHB and MEMSIZE or SHB
    return 0
  end)
  -- Flat again, now that the shards are also resident. The delta between this
  -- and c_ld_flat is the live-host-memory confound, measured rather than
  -- assumed away.
  timed("c_ld_flat2/" .. name, n, function() return ld_flat(n) end)
  timed("c_ld_divmod/" .. name, n, function() return ld_divmod(n) end)
  timed("c_ld_guard_shard/" .. name, n, function() return ld_guard_shard(n) end)
  timed("c_st_divmod/" .. name, n, function() return st_divmod(n) end)

  MEM, MEMS, SH = nil, nil, nil
  collectgarbage("collect")
end

-- THE DECISIVE ARM, and it needs a better instrument than the others.
--
-- Everything above is measured once per size, and run 1 against run 2 of this
-- probe put the same flat load at 56.1 ns and 73.4 ns -- 31% apart, in the same
-- game, at the same size, on the same machine. That spread does not matter
-- against a 33x cliff and it matters entirely against a 1.35x regression, which
-- is the number the whole design decision hangs on.
--
-- So the below-wall regression is measured PAIRED: flat, sharded, flat,
-- sharded, k times, in one run, at one size. Each pair is a ratio taken from
-- two measurements a few milliseconds apart, and the SPREAD OF THE RATIOS is
-- reported rather than a single number -- agents/benchmarks.md's rule about a
-- floor measured in the same run, applied to the one arm where the effect is
-- the same order as the noise.
local function run_paired(name, words, k)
  log("FKSHARD_SIZE\tpaired/" .. name .. "\t" .. words)
  MEM = build_flat(words)
  MEMS, SH = build_shards(words)
  S1 = MEMS[1]
  MEMSIZE = words * 4
  SHBOUND = MEMSIZE < SHB and MEMSIZE or SHB
  local n = REPS
  if n * 4 > MEMSIZE then n = math.floor(MEMSIZE / 4) end
  for j = 1, k do
    timed("p_ld_flat." .. j .. "/" .. name, n, function() return ld_flat(n) end)
    timed("p_ld_divmod." .. j .. "/" .. name, n, function() return ld_divmod(n) end)
    timed("p_ld_s0fast." .. j .. "/" .. name, n, function() return ld_s0fast(n) end)
    timed("p_ld_gflat." .. j .. "/" .. name, n, function() return ld_guard_upval(n) end)
    timed("p_ld_gshard." .. j .. "/" .. name, n, function() return ld_guard_shard(n) end)
    timed("p_st_flat." .. j .. "/" .. name, n, function() return st_flat(n) end)
    timed("p_st_divmod." .. j .. "/" .. name, n, function() return st_divmod(n) end)
    timed("p_st_s0fast." .. j .. "/" .. name, n, function() return st_s0fast(n) end)
  end
  MEM, MEMS, SH = nil, nil, nil
  collectgarbage("collect")
end

-- The ABOVE-wall win, paired and on the address pattern that is hardest for
-- every sharded form: an 8 KiB stride over the whole memory, so shard 0's fast
-- arm misses on three addresses in four at 8 MiB and the shard tables get no
-- locality help either.
local function run_paired_above(name, words, k)
  log("FKSHARD_SIZE\tabove/" .. name .. "\t" .. words)
  MEM = build_flat(words)
  MEMS, SH = build_shards(words)
  S1 = MEMS[1]
  MEMSIZE = words * 4
  SHBOUND = MEMSIZE < SHB and MEMSIZE or SHB
  local n = REPS
  for j = 1, k do
    timed("a_ld_flat." .. j .. "/" .. name, n, function() return ld_flat_spread(n) end)
    timed("a_ld_divmod." .. j .. "/" .. name, n, function() return ld_divmod_spread(n) end)
    timed("a_ld_s0fast." .. j .. "/" .. name, n, function() return ld_s0fast_spread(n) end)
  end
  MEM, MEMS, SH, S1 = nil, nil, nil, nil
  collectgarbage("collect")
end

local done = false

script.on_event(defines.events.on_tick, function()
  if done then return end
  done = true

  log("FKSHARD_BEGIN\tshardw=" .. SHW .. "\tshardb=" .. SHB .. "\treps=" .. REPS)

  -- WHERE THE WALL ACTUALLY IS. agents/gc.md states the law as "more than
  -- 2^20 = 1,048,576 keys", so 1,048,576 words (4.000 MiB) should still be an
  -- array and 1,052,672 (4.016 MiB) should not. The pair brackets it; the rest
  -- is the slope on either side, which run 1 of this probe suggested is NOT
  -- flat below the wall.
  -- The below-wall regression, paired. 1 MiB and 2 MiB are where a guest that
  -- never approaches the wall actually lives, and they are the sizes at which
  -- always-sharding can only cost.
  run_paired("1.0MiB", 262144, 7)
  run_paired("2.0MiB", 524288, 7)

  -- And the win, on the pattern that is hardest for a sharded form.
  run_paired_above("8.0MiB", 2097152, 5)

  run_curve("1.0MiB", 262144)
  run_curve("2.0MiB", 524288)
  run_curve("3.0MiB", 786432)
  run_curve("3.9MiB", 1022976)
  run_curve("4.000MiB", 1048576)
  run_curve("4.016MiB", 1052672)
  run_curve("5.0MiB", 1310720)

  -- 2 MiB: below the wall, and exactly ONE shard -- the below-wall regression
  -- case, where sharding can only cost.
  run_size("2MiB", 524288)
  -- 3.5 MiB: still below the wall, two shards, so the shard select is doing
  -- real work and the flat table is still an array.
  -- 5 MiB: just above. The flat table stops being an array here.
  -- 8 MiB: well above, the size agents/gc.md priced at 5,284 ms to build.
  run_size("8MiB", 2097152)

  log("FKSHARD_END\tsink=" .. sink)
end)
