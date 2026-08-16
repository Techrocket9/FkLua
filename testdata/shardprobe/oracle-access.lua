-- The shardprobe's ld_ctl and ld_flat arms, verbatim, under bin/lua52f.
--
-- WHY THIS EXISTS. agents/gc.md's stage-D open item 1 is a stop-the-world
-- collection costing 88 ms/MiB in game against a host-side 13.9-32.8, with the
-- recorded lead being the 4 MiB wall. agents/sharding.md §2 falsifies that lead
-- -- the arm that reproduces 88 ms/MiB never crossed the wall -- and this file
-- is the other half of the replacement: the SAME two loops, on the SAME
-- below-wall table, in the interpreter the host-side band was derived under.
-- What comes out is the constant that carries a host-side collection number
-- into the game, and nothing in this repo had ever measured it.
--
-- lua52f faithfully has no clock (agents/sandbox.md: `os` is absent, and adding
-- one would make the oracle unlike the sandbox), so the harness times the
-- PROCESS and the two modes differ by exactly one table read. OUTER is large so
-- that interpreter start-up and the table build are a small share of both legs
-- and cancel in the difference.
--
--   ./bin/lua52f testdata/shardprobe/oracle-access.lua access
--   ./bin/lua52f testdata/shardprobe/oracle-access.lua control
--
-- scripts/run-shardprobe.sh runs both and prints the pair.
local MODE = ...
local words = 524288                  -- 2 MiB: below any wall, in either Lua
local MEM = {}
for i = 1, words do MEM[i] = i % 251 end
local MEMSIZE = words * 4
local REPS = 400000                   -- the probe's own per-arm count
local OUTER = 25
local s = 0

local function trap_oob() error("oob", 0) end

if MODE == "access" then
  for _ = 1, OUTER do
    local t0
    for a = 0, (REPS - 1) * 4, 4 do
      t0 = a
      if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
      if t0 % 4 == 0 then s = s + MEM[t0 / 4 + 1] else s = s + 1 end
    end
  end
else
  for _ = 1, OUTER do
    local t0
    for a = 0, (REPS - 1) * 4, 4 do
      t0 = a
      if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
      if t0 % 4 == 0 then s = s + 1 else s = s + 1 end
    end
  end
end
print(MODE, REPS * OUTER, s)
