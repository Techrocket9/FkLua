-- Kernel 3: xorshift32. Exercises shifts, xor and 32-bit wrapping together.
--
-- Source program being modelled:
--   uint32_t x;
--   x ^= x << 13; x ^= x >> 17; x ^= x << 5;
--
-- This is the honest worst case for the arithmetic-over-bit32 strategy, since
-- xor has no arithmetic form and must go through bit32 in both variants. What
-- differs is the shifts: "gen" uses the measured-fastest lowerings, "nat" uses
-- bit32.lshift/rshift as a mod author naturally would.
--
-- Usage: prng.lua <gen|nat> [iterations]

local argv = { ... }
local variant = argv[1]
local N = tonumber(argv[2]) or 4000000

local checksum, ops

if variant == "gen" then
  local bxor = bit32.bxor
  local x = 2463534242.0
  local t
  for _ = 1, N do
    -- x ^= x << 13   -- shl masks first: (a % 2^(32-n)) * 2^n
    t = (x % 524288.0) * 8192.0
    x = bxor(x, t)
    -- x ^= x >> 17   -- shr_u as (a - a%2^n)/2^n, measured 2.6x faster than floor
    t = (x - x % 131072.0) / 131072.0
    x = bxor(x, t)
    -- x ^= x << 5
    t = (x % 134217728.0) * 32.0
    x = bxor(x, t)
  end
  checksum, ops = x, N

elseif variant == "nat" then
  local bxor, lshift, rshift = bit32.bxor, bit32.lshift, bit32.rshift
  local x = 2463534242
  for _ = 1, N do
    x = bxor(x, lshift(x, 13))
    x = bxor(x, rshift(x, 17))
    x = bxor(x, lshift(x, 5))
  end
  checksum, ops = x, N

else
  error("usage: prng.lua <gen|nat> [iterations]")
end

print(string.format("%.0f\t%d", checksum, ops))
