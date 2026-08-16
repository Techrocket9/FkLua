-- Kernel 5: f64 dot product. Isolates the cost of getting doubles in and out of
-- linear memory, which is where the word-table representation hurts most.
--
-- Source program being modelled:
--   double dot(double *a, double *b, int n) {
--     double s = 0; for (int i = 0; i < n; i++) s += a[i] * b[i]; return s;
--   }
--
-- An f64 occupies two u32 words, so every load is two table reads plus an
-- IEEE-754 reassembly. "gen" therefore pays a real, unavoidable cost that "nat"
-- does not -- Lua stores a double natively in one slot.
--
-- Two gen variants are measured, because the difference between them decides
-- whether the M5 "typed-slot promotion" pass is worth building:
--   gen      -- values genuinely reassembled from two words, the honest cost
--   genslot  -- values already promoted into Lua locals, i.e. the pass's payoff
--
-- Usage: dot.lua <gen|genslot|nat> [elements] [passes]

local argv = { ... }
local variant = argv[1]
local N = tonumber(argv[2]) or 32768
local R = tonumber(argv[3]) or 40

local ldexp = math.ldexp
local checksum, ops

-- Reassemble an IEEE-754 double from its two u32 halves. This is the cost that
-- typed-slot promotion exists to avoid.
local function f64_from(lo, hi)
  local sign = 1.0
  if hi >= 2147483648.0 then sign = -1.0; hi = hi - 2147483648.0 end
  local expo = (hi - hi % 1048576.0) / 1048576.0
  local mant = (hi % 1048576.0) * 4294967296.0 + lo
  if expo == 0 then
    return sign * ldexp(mant, -1074)
  end
  return sign * ldexp(mant + 4503599627370496.0, expo - 1075)
end

if variant == "gen" or variant == "genslot" then
  -- Two f64 arrays in linear memory, a[] then b[], 8 bytes per element.
  local MEM = {}
  local words = N * 4
  for i = 1, words do MEM[i] = 0 end
  -- Fill with the bit patterns of known doubles via frexp, mirroring how a
  -- store would have written them.
  local function store_f64(word_index, x)
    local m, e = math.frexp(x)
    local sign = 0.0
    if m < 0 then m = -m; sign = 2147483648.0 end
    local mm = (m * 2.0 - 1.0) * 4503599627370496.0
    local lo = mm % 4294967296.0
    local hi = (mm - lo) / 4294967296.0 + (e + 1022) * 1048576.0 + sign
    MEM[word_index + 1] = lo
    MEM[word_index + 2] = hi
  end
  for i = 0, N - 1 do
    store_f64(i * 2, 1.0 + i * 0.000030517578125)
    store_f64(N * 2 + i * 2, 2.0 - i * 0.000015258789062)
  end

  if variant == "gen" then
    local function fk_dot(pa, pb, n)
      local l0, l1, lo, hi, x, y
      l0 = 0.0
      l1 = 0
      ::L0::
      if l1 >= n then goto L1 end
      lo = MEM[pa * 0.25 + 1]; hi = MEM[pa * 0.25 + 2]
      x = f64_from(lo, hi)
      lo = MEM[pb * 0.25 + 1]; hi = MEM[pb * 0.25 + 2]
      y = f64_from(lo, hi)
      l0 = l0 + x * y
      pa = pa + 8
      pb = pb + 8
      l1 = l1 + 1
      goto L0
      ::L1::
      return l0
    end
    local acc = 0.0
    for _ = 1, R do acc = acc + fk_dot(0, N * 8, N) end
    checksum, ops = acc, R * N
  else
    -- Typed-slot promotion: the values were never spilled to linear memory, so
    -- the loop reads Lua locals out of shadow arrays instead of reassembling.
    local A, B = {}, {}
    for i = 1, N do
      A[i] = 1.0 + (i - 1) * 0.000030517578125
      B[i] = 2.0 - (i - 1) * 0.000015258789062
    end
    local function fk_dot_slots(n)
      local l0, l1
      l0 = 0.0
      l1 = 1
      ::L0::
      if l1 > n then goto L1 end
      l0 = l0 + A[l1] * B[l1]
      l1 = l1 + 1
      goto L0
      ::L1::
      return l0
    end
    local acc = 0.0
    for _ = 1, R do acc = acc + fk_dot_slots(N) end
    checksum, ops = acc, R * N
  end

elseif variant == "nat" then
  local a, b = {}, {}
  for i = 1, N do
    a[i] = 1.0 + (i - 1) * 0.000030517578125
    b[i] = 2.0 - (i - 1) * 0.000015258789062
  end
  local function dot(x, y, n)
    local s = 0.0
    for i = 1, n do s = s + x[i] * y[i] end
    return s
  end
  local acc = 0.0
  for _ = 1, R do acc = acc + dot(a, b, N) end
  checksum, ops = acc, R * N

else
  error("usage: dot.lua <gen|genslot|nat> [elements] [passes]")
end

print(string.format("%.6f\t%d", checksum, ops))
