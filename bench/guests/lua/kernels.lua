-- The cross-language benchmark, hand-written Lua side.
--
-- These are what a Factorio mod author writes INSTEAD of using FkLua, so they
-- are deliberately idiomatic rather than shaped to resemble emitter output:
-- plain tables, numeric `for`, string concatenation, no wrapping arithmetic
-- where Lua's doubles make it unnecessary. bench/kernels/ already holds the
-- other thing -- Lua written to model what the emitter produces.
--
-- The contract with the guests: same work, same checksum. The harness compares
-- checksums before it reports a single timing, because a variant that computes
-- a different answer is not a faster variant.
--
-- One asymmetry is deliberate and worth stating. A guest wraps at 2^32 because
-- wasm says so; this code wraps only where the checksum depends on it. Adding
-- `% 4294967296` to every operation here would be modelling a guest rather than
-- writing Lua, and would flatter FkLua by slowing down the thing it is measured
-- against.

local M = {}

-- ---------------------------------------------------------------------------
-- PURE
-- ---------------------------------------------------------------------------

local words = {}

function M.pure_setup(n)
  words = {}
  for i = 0, n - 1 do words[i + 1] = (i * 2654435761) % 4294967296 end
end

function M.pure_sum(passes)
  local acc = 0
  local w, n = words, #words
  for _ = 1, passes do
    local s = 0
    for i = 1, n do s = s + w[i] end
    acc = (acc + s) % 4294967296
  end
  return acc
end

-- xorshift32 with bit32, which is what a mod author reaches for. This is the
-- kernel FkLua already beats outright: bit32.bxor is a function call, where the
-- emitter lowers a shift to a multiply and a floor.
local bxor, lshift, rshift = bit32.bxor, bit32.lshift, bit32.rshift

function M.pure_prng(n)
  local x = 2463534242
  for _ = 1, n do
    x = bxor(x, lshift(x, 13))
    x = bxor(x, rshift(x, 17))
    x = bxor(x, lshift(x, 5))
  end
  return x
end

local fa, fb = {}, {}

function M.dot_setup(n)
  fa, fb = {}, {}
  for i = 0, n - 1 do
    fa[i + 1] = i * 0.5
    fb[i + 1] = i * 0.25 + 1.0
  end
end

function M.pure_dot(passes)
  local acc = 0.0
  local a, b, n = fa, fb, #fa
  for _ = 1, passes do
    local s = 0.0
    for i = 1, n do s = s + a[i] * b[i] end
    acc = acc + s
  end
  return acc
end

-- ---------------------------------------------------------------------------
-- REALISTIC
-- ---------------------------------------------------------------------------

-- An array of records. A mod author writes a table per entity with named
-- fields -- that is the idiomatic form, and it is what makes this comparison
-- honest: a Lua table field is a hash lookup where the guest's struct field is
-- an offset, so this is the kernel where Lua pays and the guest does not.
local ents = {}

function M.ents_setup(n)
  ents = {}
  for i = 0, n - 1 do
    ents[i + 1] = {
      kind    = i % 7,
      active  = i % 3 ~= 0,
      x       = (i % 512) - 256,
      y       = math.floor(i / 512) - 256,
      amount  = (i * 2654435761) % 4294967296 % 1000,
      quality = i % 5,
    }
  end
end

function M.real_entities(passes)
  local acc = 0
  local es, n = ents, #ents
  for _ = 1, passes do
    local totals = {0, 0, 0, 0, 0, 0, 0}
    for i = 1, n do
      local e = es[i]
      if e.active and e.quality ~= 0 and e.x >= -128 and e.x <= 128 then
        local k = e.kind + 1
        totals[k] = (totals[k] + e.amount) % 4294967296
      end
    end
    for k = 1, 7 do acc = (acc * 31 + totals[k]) % 4294967296 end
  end
  return acc
end

local grid = {}

function M.grid_setup(side)
  grid = {}
  for i = 0, side * side - 1 do
    local h = (i * 2654435761) % 4294967296
    grid[i + 1] = (math.floor(h / 65536) % 10 < 3) and 1 or 0
  end
end

function M.real_grid(side, passes)
  local acc = 0
  local g = grid
  local n = side * side
  local seen, stack = {}, {}
  for _ = 1, passes do
    for i = 1, n do seen[i] = 0 end
    local top = 0
    local filled = 0
    -- The first open cell at or after the centre. The centre itself is a wall
    -- on this maze, and starting there made every language agree on a checksum
    -- of zero -- agreement about doing no work.
    local start = math.floor(side / 2) * side + math.floor(side / 2)
    while start < n and g[start + 1] ~= 0 do start = start + 1 end
    if start < n then
      top = 1; stack[1] = start; seen[start + 1] = 1
    end
    while top > 0 do
      local cur = stack[top]
      top = top - 1
      filled = filled + 1
      local cx = cur % side
      local cy = math.floor(cur / side)
      if cx > 0 then
        local m = cur - 1
        if g[m + 1] == 0 and seen[m + 1] == 0 then
          seen[m + 1] = 1; top = top + 1; stack[top] = m
        end
      end
      if cx < side - 1 then
        local m = cur + 1
        if g[m + 1] == 0 and seen[m + 1] == 0 then
          seen[m + 1] = 1; top = top + 1; stack[top] = m
        end
      end
      if cy > 0 then
        local m = cur - side
        if g[m + 1] == 0 and seen[m + 1] == 0 then
          seen[m + 1] = 1; top = top + 1; stack[top] = m
        end
      end
      if cy < side - 1 then
        local m = cur + side
        if g[m + 1] == 0 and seen[m + 1] == 0 then
          seen[m + 1] = 1; top = top + 1; stack[top] = m
        end
      end
    end
    acc = (acc * 31 + filled) % 4294967296
  end
  return acc
end

-- Building and hashing names. The kernel a compiled guest should LOSE: Lua's
-- strings are C-implemented and interned, and `..` on two short strings is a
-- memcpy the interpreter does for free, where a guest assembles bytes in linear
-- memory through its own allocator.
local byte, sub = string.byte, string.sub

function M.real_names(n)
  local acc = 0
  for i = 0, n - 1 do
    local s = "iron-plate-" .. i
    local h = 2166136261
    for k = 1, #s do
      h = bxor(h, byte(s, k))
      -- h * 16777619 mod 2^32, SPLIT so no intermediate exceeds 2^53.
      --
      -- Written the obvious way -- `h = (h * 16777619) % 4294967296` -- this is
      -- silently wrong: h reaches 2^32, and 2^32 * 16777619 is 7.2e16 against a
      -- double's 9.0e15 exact-integer ceiling. It produces a plausible number
      -- and the wrong one, which is how the checksum comparison caught it.
      --
      -- A guest never has this problem. wasm says the multiply is 32-bit and
      -- the emitter lowers it accordingly, so the same source in Go or Rust is
      -- correct without anyone thinking about it. That asymmetry is a real part
      -- of the answer to "should I write this in Lua", and it is not a speed
      -- number.
      local lo = h % 65536
      local hi = (h - lo) / 65536
      h = (lo * 16777619 + ((hi * 16777619) % 65536) * 65536) % 4294967296
    end
    acc = (acc * 31 + h) % 4294967296
  end
  return acc
end

return M
