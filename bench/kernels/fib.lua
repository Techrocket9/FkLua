-- Kernel 6: recursive fib(30). ~2.7M calls, so this measures call dispatch
-- almost exclusively.
--
-- Three variants, because the difference between the two gen forms is exactly
-- the payoff of upvalue promotion -- which the in-game probe already showed to
-- be worth 27%, over the 25% threshold that moves it into v1:
--   gen     -- F[idx] table dispatch, what a naive emitter produces
--   genup   -- the same code with the callee promoted to an upvalue
--   nat     -- an ordinary local recursive function
--
-- Usage: fib.lua <gen|genup|nat> [n]

local argv = { ... }
local variant = argv[1]
local N = tonumber(argv[2]) or 30

local checksum, calls

-- fib(n) makes 2*fib(n+1)-1 calls; counting them exactly keeps ns/op honest
-- rather than dividing by an invented number.
local function callcount(n)
  local a, b = 1, 1            -- fib(0), fib(1)
  for _ = 2, n do a, b = b, a + b end
  local fibn1 = a + b          -- fib(n+1)
  if n == 0 or n == 1 then fibn1 = 2 end
  return 2 * fibn1 - 1
end

if variant == "gen" then
  -- Functions live in a table: the chunk is itself a function and caps at 200
  -- locals, so `local f0, f1, ... fN` is not available to the emitter.
  local F = {}
  F[1] = function(n)
    local l0, l1
    if n < 2 then return n end
    l0 = F[1]((n - 1) % 4294967296.0)
    l1 = F[1]((n - 2) % 4294967296.0)
    return (l0 + l1) % 4294967296.0
  end
  checksum, calls = F[1](N), callcount(N)

elseif variant == "genup" then
  -- Same emitted shape, but the hot callee has been promoted to an upvalue.
  local F = {}
  local fib
  fib = function(n)
    local l0, l1
    if n < 2 then return n end
    l0 = fib((n - 1) % 4294967296.0)
    l1 = fib((n - 2) % 4294967296.0)
    return (l0 + l1) % 4294967296.0
  end
  F[1] = fib
  checksum, calls = fib(N), callcount(N)

elseif variant == "nat" then
  local function fib(n)
    if n < 2 then return n end
    return fib(n - 1) + fib(n - 2)
  end
  checksum, calls = fib(N), callcount(N)

else
  error("usage: fib.lua <gen|genup|nat> [n]")
end

print(string.format("%.0f\t%d", checksum, calls))
