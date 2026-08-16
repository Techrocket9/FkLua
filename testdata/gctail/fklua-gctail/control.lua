-- FkLua Lua-GC tail probe.
--
-- WHY A BARE LUA MOD, AND WHY IN THE GAME. agents/guests.md's cost table has
-- two rows it cannot explain: at a 52 MiB sharded heap one 24,000-tick run
-- found a 4.178 ms worst `luaGarbageIncremental` where a 54.5 MiB run of the
-- same guest found 1.141 ms. Both are far above the ~0.5 ms a single shard's
-- `propagatemark` costs, and the file records the honest conclusion -- "not a
-- hard bound" -- plus a hypothesis: Lua's ATOMIC step, the one phase of
-- `luaGarbageIncremental` that is not incremental at all.
--
-- Testing that needs three quantities varied INDEPENDENTLY, and through a wasm
-- guest they are welded together: total bytes, table COUNT and table SIZE all
-- move at once when a heap grows. Here they are three numbers in a config
-- file. Everything else follows the instrument rule this repo already has for
-- the wall, the shard probe and the grow probe: `bin/lua52f` is stock 5.2.1
-- and prices Factorio's table internals wrong in both directions, so the
-- measurement is in the game, in a mod with no wasm, no emitter, no --persist
-- and no collector in it.
--
-- WHAT IS MEASURED. Nothing in here is timed by this mod. The instrument is
-- Factorio's own `--benchmark-verbose luaGarbageIncremental`, one row per
-- tick, which is the same counter the cost table's rows come from -- so this
-- probe and the table it is auditing are reading the same clock.
--
-- WHAT THIS MOD IS, in three lines:
--
--   * it holds `shards` live Lua array tables of `shardw` zeroed words each,
--     which is the shape a guest's linear memory has since sharding stage B;
--   * it allocates a bounded amount of SHORT-LIVED garbage every tick, because
--     Lua's collector advances on allocation debt and a mod that allocates
--     nothing never steps -- an idle probe would measure zero and prove
--     nothing;
--   * it optionally drives `collectgarbage` itself, which is the mitigation
--     lever agents/sandbox.md says round 5 confirmed is available.
--
-- THE SHARDS ARE BUILT AT CHUNK LOAD, deliberately, and not in on_init or a
-- one-shot on_tick. Building 52 MiB of table is seconds of work; at chunk load
-- it lands before Factorio's first `t0` row and is therefore outside the
-- window entirely, where in on_tick it would be the largest row in the file and
-- would be measuring itself. They are held in a module local rather than in
-- `storage` for a second reason of the same kind: `storage` would put 52 MiB
-- into every save and every benchmark's LOAD, which is a cost this probe is not
-- about. A chunk upvalue is as live, and as walked, as a saved table.

local C = require("config")

local SHARDS = C.shards       -- how many tables
local SHARDW = C.shardw       -- words in each
local CHURN = C.churn         -- short-lived tables allocated per tick
local CHURNW = C.churnw       -- words in each of those
local PACE = C.pace           -- 0 = leave Lua's collector alone
local WRITES = C.writes or 0  -- words STORED INTO the live set per tick
local WSHARDS = C.wshards or 0 -- how many DISTINCT shards those writes reach

-- ---------------------------------------------------------------------------
-- The live set: `SHARDS` array tables of `SHARDW` zeros.
--
-- Filled ascending, one word at a time, exactly as runtime/lua/fk_rt.lua's
-- mem_grow fills a shard -- so the array part is grown by Lua's own doubling
-- ladder and each table ends up in the representation a real guest's shard is
-- in. A table built any other way could differ in `sizearray` versus hash-part
-- occupancy, which is precisely what `traversestrongtable` walks.
-- ---------------------------------------------------------------------------
local MEM = {}
for s = 1, SHARDS do
  local t = {}
  for i = 1, SHARDW do t[i] = 0 end
  MEM[s] = t
end

-- A sink nothing can fold away, so the churn below cannot be optimised out and
-- a mis-built live set shows up as an error rather than as a fast number.
local sink = 0
for s = 1, SHARDS do
  local t = MEM[s]
  if t[1] ~= 0 or t[SHARDW] ~= 0 then
    error("gctail: shard " .. s .. " is not zeroed", 0)
  end
  sink = sink + t[SHARDW]
end

log("FKGCTAIL_SETUP shards=" .. SHARDS .. " shardw=" .. SHARDW ..
    " words=" .. (SHARDS * SHARDW) ..
    " mib=" .. string.format("%.1f", SHARDS * SHARDW * 4 / 1048576) ..
    " churn=" .. CHURN .. "x" .. CHURNW .. " pace=" .. PACE ..
    " writes=" .. WRITES .. " wshards=" .. WSHARDS)

-- ---------------------------------------------------------------------------
-- The per-tick load.
--
-- CHURN short-lived tables of CHURNW words each. This is the allocation debt
-- that makes Lua's collector step at all; without it `luaGarbageIncremental` is
-- flat zero and the probe measures nothing. It is deliberately SMALL and
-- deliberately CONSTANT across arms, so that the only thing differing between
-- two arms is the live set they are being asked to traverse.
-- ---------------------------------------------------------------------------
local ticks = 0

-- WRITING INTO THE LIVE SET IS THE WHOLE MECHANISM, and the first version of
-- this probe did not do it -- which is why its tail happened exactly ONCE per
-- run and then never again.
--
-- Lua 5.2 marks a table BLACK once `traversestrongtable` has walked it. A store
-- into a black table fires `luaC_barrierback`, which puts the table back on the
-- GRAYAGAIN list; and `atomic()` -- the one phase of an "incremental" collection
-- that is not incremental at all -- ends by calling `propagateall` over that
-- list, traversing every table on it inside ONE indivisible step. So the size of
-- the atomic tick is not the heap: it is the part of the heap that was WRITTEN
-- since the last one.
--
-- That predicts the thing agents/guests.md could not explain -- two runs of the
-- same guest at the same heap size disagreeing by 3.7x -- as a difference in how
-- many distinct SHARDS the guest happened to touch per cycle. WSHARDS is that
-- number, made into a knob, so the prediction is tested rather than told.
--
-- The stores are strided rather than random: determinism is a correctness
-- property in this repo and a probe with an RNG in it is a probe whose two runs
-- are not the same experiment.
local wcursor = 0

script.on_event(defines.events.on_tick, function()
  ticks = ticks + 1
  for _ = 1, CHURN do
    local g = {}
    for i = 1, CHURNW do g[i] = i end
    sink = sink + g[CHURNW]
  end

  -- WRITES words spread over the first WSHARDS shards. Every one of them is a
  -- back-barrier on a table that is black by now, so this is exactly WSHARDS
  -- tables re-grayed per tick.
  if WRITES > 0 and WSHARDS > 0 then
    for i = 1, WRITES do
      local s = (wcursor + i) % WSHARDS + 1
      local t = MEM[s]
      local k = (wcursor * 7919 + i * 104729) % SHARDW + 1
      t[k] = ticks
    end
    wcursor = wcursor + WRITES
  end

  -- THE PACING LEVER. `collectgarbage` is in Factorio's sandbox with every
  -- option (agents/sandbox.md), and agents/guests.md records that it "moves the
  -- pause by less than its own noise, because there is nothing to pace" -- a
  -- claim made BEFORE sharding, when the memory was one indivisible table. With
  -- a vector of shards there is something to pace, so the claim is worth
  -- retaking rather than inheriting.
  --
  -- "step" with an explicit size is the only option that asks for a BOUNDED
  -- amount of work; setpause/setstepmul change the rate at which Lua asks for
  -- its own. All three are arms rather than a recommendation.
  if PACE > 0 then
    collectgarbage("step", PACE)
  end

  if ticks % 4000 == 0 then
    log("FKGCTAIL tick=" .. ticks .. " count=" .. collectgarbage("count") ..
        " sink=" .. sink)
  end
end)

log("FKGCTAIL_READY count=" .. collectgarbage("count"))
