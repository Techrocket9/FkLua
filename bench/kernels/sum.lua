-- Kernel 1: sum a u32 array. THE gate kernel.
--
-- Source program being modelled:
--   uint32_t sum(uint32_t *a, int n) {
--     uint32_t s = 0;
--     for (int i = 0; i < n; i++) s += a[i];
--     return s;
--   }
--
-- "gen" is what FkLua emits: linear memory as a 1-based table of u32 words,
-- wrapping i32 arithmetic, all locals declared in the prologue, flat goto
-- control flow. "nat" is what a mod author would write instead. The ratio
-- between them is the project's reason to exist -- if it is bad enough that
-- writing Lua by hand is obviously better, there is nothing here worth building.
--
-- Usage: sum.lua <gen|nat> [words] [passes]

local argv = { ... }
local variant = argv[1]
local W = tonumber(argv[2]) or 262144              -- 1 MiB guest heap
local R = tonumber(argv[3]) or 40

local checksum, ops

if variant == "gen" then
  -- Linear memory: one Lua table of u32 words, 1-based.
  local MEM = {}
  for i = 1, W do MEM[i] = (i * 2654435761) % 4294967296 end

  -- The emitted function. Locals are declared once, up front (Invariant B), so
  -- no goto can jump into a local's scope.
  local function fk_sum(p, n)
    local l0, l1, s0
    l0 = 0                                        -- uint32_t s
    l1 = 0                                        -- int i
    ::L0::
    if l1 >= n then goto L1 end
    s0 = MEM[p * 0.25 + 1]                        -- i32.load, aligned, const-folded
    l0 = (l0 + s0) % 4294967296.0                 -- i32.add + wrap
    p = (p + 4) % 4294967296.0                    -- strength-reduced pointer bump
    l1 = (l1 + 1) % 4294967296.0                  -- i++
    goto L0
    ::L1::
    return l0
  end

  local acc = 0
  for _ = 1, R do acc = (acc + fk_sum(0, W)) % 4294967296.0 end
  checksum, ops = acc, R * W

elseif variant == "nat" then
  -- What a Factorio mod author writes: a plain Lua array and a numeric for.
  -- No wrapping, because Lua numbers simply do not overflow at these sizes.
  local a = {}
  for i = 1, W do a[i] = (i * 2654435761) % 4294967296 end

  local function sum(t, n)
    local s = 0
    for i = 1, n do s = s + t[i] end
    return s
  end

  local acc = 0
  for _ = 1, R do acc = (acc + sum(a, W)) % 4294967296.0 end
  checksum, ops = acc, R * W

else
  error("usage: sum.lua <gen|nat> [words] [passes]")
end

print(string.format("%.0f\t%d", checksum, ops))
