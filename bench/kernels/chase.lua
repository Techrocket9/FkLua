-- Kernel 4: pointer chase over a linked list. The other gate kernel.
--
-- Source program being modelled:
--   struct node { uint32_t next; uint32_t val; };
--   uint32_t walk(struct node *p) {
--     uint32_t s = 0;
--     while (p) { s += p->val; p = (struct node *)p->next; }
--     return s;
--   }
--
-- This is the pattern the linear-memory design is worst at and that idiomatic
-- Lua is best at: "gen" must compute a byte address and index a flat word table
-- for every field, where "nat" simply follows a table reference. If the ratio
-- is acceptable here it is acceptable for struct-heavy code generally.
--
-- The list is built in a shuffled order so the access pattern is not
-- sequential; a purely ascending chase would let the cache flatter the flat
-- array unfairly relative to the pointer-per-node version.
--
-- Usage: chase.lua <gen|nat> [nodes] [passes]

local argv = { ... }
local variant = argv[1]
local N = tonumber(argv[2]) or 65536
local R = tonumber(argv[3]) or 30

-- A deterministic permutation, so both variants walk the same order. Coprime
-- stride over a power-of-two length visits every slot exactly once.
local STRIDE = 40503
local function order(i) return (i * STRIDE) % N end

local checksum, ops

if variant == "gen" then
  -- Nodes live in linear memory: 8 bytes each, {next: u32, val: u32}.
  -- Node k occupies byte address k*8, i.e. words k*2 and k*2+1 (1-based: +1).
  local MEM = {}
  for i = 1, N * 2 do MEM[i] = 0 end
  for k = 0, N - 1 do
    local this, nxt = order(k), order((k + 1) % N)
    MEM[this * 2 + 1] = nxt * 8                   -- ->next, as a byte address
    MEM[this * 2 + 2] = (this * 2654435761) % 4294967296  -- ->val
  end

  local function fk_walk(p, n)
    local l0, l1, s0
    l0 = 0                                        -- uint32_t s
    l1 = 0                                        -- iteration guard
    ::L0::
    if l1 >= n then goto L1 end
    s0 = MEM[p * 0.25 + 2]                        -- i32.load offset=4  -> ->val
    l0 = (l0 + s0) % 4294967296.0
    p = MEM[p * 0.25 + 1]                         -- i32.load offset=0  -> ->next
    l1 = (l1 + 1) % 4294967296.0
    goto L0
    ::L1::
    return l0
  end

  local acc = 0
  for _ = 1, R do acc = (acc + fk_walk(order(0) * 8, N)) % 4294967296.0 end
  checksum, ops = acc, R * N

elseif variant == "nat" then
  -- What a mod author writes: a table per node, following references.
  local nodes = {}
  for k = 0, N - 1 do
    nodes[order(k)] = { val = (order(k) * 2654435761) % 4294967296 }
  end
  for k = 0, N - 1 do
    nodes[order(k)].next = nodes[order((k + 1) % N)]
  end

  local function walk(p, n)
    local s = 0
    for _ = 1, n do
      s = s + p.val
      p = p.next
    end
    return s
  end

  local acc = 0
  for _ = 1, R do acc = (acc + walk(nodes[order(0)], N)) % 4294967296.0 end
  checksum, ops = acc, R * N

else
  error("usage: chase.lua <gen|nat> [nodes] [passes]")
end

print(string.format("%.0f\t%d", checksum, ops))
