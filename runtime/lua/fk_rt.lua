-- FkLua runtime prelude, emitted at the top of every generated chunk.
--
-- Everything here is either a constant table the emitter indexes into or a
-- helper too large to inline. The lowering choices are measured, not assumed --
-- see bench/baselines/probe-2.0.77.json and the codegen table in CLAUDE.md.
--
-- Invariant A: an i32 is an UNSIGNED integral double in [0, 2^32). Every value
-- crossing these helpers obeys that.

-- Localised in the prologue so a call is OP_GETUPVAL + OP_CALL rather than
-- OP_GETTABUP + OP_GETTABLE + OP_CALL. bit32 is the slowest option for almost
-- everything (19.15 ns for a masked add against 2.81 for %), so the emitter
-- reaches for these only where arithmetic has no equivalent: general and/or/xor.
local frexp, huge, sqrt = math.frexp, math.huge, math.sqrt
local band, bor, bxor = bit32.band, bit32.bor, bit32.bxor
local char, concat, byte = string.char, table.concat, string.byte

-- Powers of two, 0..32. Indexed rather than computed because `^` is OP_POW,
-- which calls pow(); a table read is one VM instruction.
local P2 = {}
do
  local v = 1
  for i = 0, 32 do P2[i] = v; v = v * 2 end
end

-- PE[e] is 2^(e-1075), for e a BIASED f64 exponent in [1, 2046].
--
-- It exists to turn every `ldexp(mant, e - 1075)` in the f64 paths into one
-- multiply. `ldexp` is a C call; this is a table read and an OP_MUL, and on
-- `pure_dot` -- 4x-unrolled, eight f64 loads per iteration -- replacing it is
-- worth 0.902x measured under bin/lua52f against a 0.4% A/A floor. Same
-- reasoning as P2 above, one width up.
--
-- EXACT, not approximately so. Scaling by a power of two only changes a
-- double's exponent, so `mant * PE[e]` and `ldexp(mant, e-1075)` are the same
-- single operation. Verified under bin/lua52f over every biased exponent the
-- callers can reach (1..2046) crossed with corner and random mantissas --
-- 65,472 pairs, zero mismatches.
--
-- BUILT ITERATIVELY, and that is not style. `2 ^ (e - 1075)` is OP_POW, i.e.
-- libm's pow(), whose exactness for a power of two is not something the spec
-- promises and not something this project may assume: generated code runs in
-- lockstep multiplayer, so a table that differed by one ulp between platforms
-- would be a desync. Repeated halving and doubling from 1.0 is exact by
-- construction, including all the way down into the subnormals.
--
-- The index is the biased exponent itself rather than the unbiased one, so the
-- table is a dense ARRAY part. Indexing it from -1074 would put every entry in
-- the hash part and give back most of what it saves.
--
-- Every caller must have excluded e == 0 and e == 2047 first; each one does,
-- because subnormals, infinities and NaNs need their own arithmetic anyway.
local PE = {}
do
  local v = 1.0
  for e = 0, 1074 do PE[1075 - e] = v; v = v / 2 end
  v = 1.0
  for e = 1, 971 do v = v * 2; PE[1075 + e] = v end
end

-- Population count for one byte. i32.popcnt is four lookups and three adds.
-- The SWAR trick would need four bit32 calls, which measured 6.8x slower than
-- arithmetic on every other operation.
local PC = {}
do
  for i = 0, 255 do
    local n, v = 0, i
    while v > 0 do
      n = n + v % 2
      v = (v - v % 2) / 2
    end
    PC[i] = n
  end
end

-- Traps.
--
-- error() with level 0 suppresses Lua's "file:line:" prefix, and the payload is
-- a preallocated table rather than a string so nothing is allocated or
-- formatted on the trapping path. The table is shared: a trap carries a code,
-- not a stack, and the code is all any handler inspects.
--
-- __tostring is what makes a trap survive the trip out of the guest. A handler
-- that does not know about fk_trap -- Factorio's own error reporting, which is
-- the one that matters -- prints the error object, and a bare table prints as
-- "(error object is not a string)", losing the only useful fact. The metamethod
-- costs nothing until something actually formats one.
local TRAPMETA = {
  __tostring = function(e) return "fklua trap: " .. e.fk_trap end,
}
local function trap(code) return setmetatable({ fk_trap = code }, TRAPMETA) end

-- Every trap the runtime can raise, in one table.
--
-- One chunk-level local instead of nine. Chunk locals are the scarcest resource
-- a generated chunk has -- the prelude takes most of Lua's 200, and a guest's
-- globals and its promoted upvalues compete for what is left -- and a table
-- costs an extra OP_GETTABLE only on the trap path, which is terminal and so
-- never hot. Having the whole set in one place is a readability win too.
local TRAPS = {
  div0        = trap("integer divide by zero"),
  overflow    = trap("integer overflow"),
  unreachable = trap("unreachable"),
  -- Not a wasm trap: the spec has no instruction budget and a conforming module
  -- may loop forever. This is a HOST policy, because the host is a lockstep game
  -- with no way to interrupt a running mod -- an infinite guest loop hangs every
  -- player's client until they kill the process.
  fuel        = trap("out of fuel"),
  oob         = trap("out of bounds memory access"),
  indirect    = trap("indirect call type mismatch"),
  uninit      = trap("undefined element"),
  -- i32.trunc_* traps rather than saturating: on NaN, and on anything outside
  -- the destination range. The saturating forms are separate opcodes.
  nan         = trap("invalid conversion to integer"),
  range       = trap("integer overflow"),
}

local function trap_fuel() error(TRAPS.fuel, 0) end

-- Distinct from a wasm trap on purpose: this means FkLua has not implemented
-- something, not that the guest program did something illegal. The test harness
-- counts it as skipped rather than failed, so unimplemented features never
-- masquerade as working ones.
local UNSUPMETA = {
  __tostring = function(e) return "fklua: unimplemented: " .. e.fk_unsupported end,
}
local function unsupported(why)
  error(setmetatable({ fk_unsupported = why }, UNSUPMETA), 0)
end

local function trap_div0() error(TRAPS.div0, 0) end
local function trap_overflow() error(TRAPS.overflow, 0) end
local function trap_unreachable() error(TRAPS.unreachable, 0) end

-- Bind one host import, or refuse to load.
--
-- A missing import is caught here, at instantiation, rather than at the first
-- call: the guest may not reach it until some rare event fires, and "attempt to
-- call a nil value" at tick 400000 does not say which import was never bound.
-- The signature is in the message because the other half of the mistake is
-- binding a function of the wrong arity, which Lua will not complain about at
-- all.
local function fk_import(t, mod, name, sig)
  local m = type(t) == "table" and t[mod]
  local f = type(m) == "table" and m[name]
  if type(f) ~= "function" then
    error("fklua: host import " .. mod .. "." .. name .. " " .. sig ..
          " was not supplied", 0)
  end
  return f
end

-- i32.mul, general case.
--
-- A full 32x32 product reaches 2^64 and would lose precision in a double, whose
-- mantissa holds only 53 bits. Splitting both operands into 16-bit halves keeps
-- every partial product exact.
--
-- The floors use the magic-number trick: adding and subtracting 2^52+2^51
-- forces the FPU to round to an integer, with no C call at all. Measured 54.0
-- ns against 62.2 for math.floor and 111.9 for bit32, and verified exact in
-- Factorio across the input domain (probe item magic_floor_correct).
--
-- The emitter inlines this when a mul is hot; the function exists for the
-- general case, where the call is cheaper than the code size.
-- MAGIC and INV65536 sit inside the do-block, which costs the CHUNK nothing:
-- a block-scoped local is not live at chunk level, and chunk-level names are the
-- scarcest thing a generated chunk has. floor32 and mul32 stay chunk-level
-- because the i64 helpers further down call them.
local floor32, mul32
do
  local MAGIC = 6755399441055744.0    -- 2^52 + 2^51
  local INV65536 = 1.52587890625e-05  -- 2^-16, exact

  -- Floor via the magic-number trick: adding and subtracting 2^52+2^51 forces
  -- the FPU to round to an integer with no C call at all.
  function floor32(q)
    local t = q + MAGIC - MAGIC
    if t > q then return t - 1.0 end
    return t
  end

  function mul32(a, b)
    local q = a * INV65536
    local ah = q + MAGIC - MAGIC; if ah > q then ah = ah - 1.0 end
    local al = a - ah * 65536.0

    local q2 = b * INV65536
    local bh = q2 + MAGIC - MAGIC; if bh > q2 then bh = bh - 1.0 end
    local bl = b - bh * 65536.0

    -- Only the low 32 bits survive, so ah*bh (which starts at bit 32) is
    -- dropped entirely and the cross terms are truncated to 16 bits.
    local m = al * bh + ah * bl
    local q3 = m * INV65536
    local mh = q3 + MAGIC - MAGIC; if mh > q3 then mh = mh - 1.0 end

    local r = al * bl + (m - mh * 65536.0) * 65536.0
    return r % 4294967296.0
  end
end

-- i32.div_s / i32.rem_s.
--
-- Computed on magnitudes so every intermediate stays a non-negative integer
-- below 2^31 and therefore exact. Dividing the signed values directly would
-- rely on IEEE division rounding the way we want, which it does not always do.
local function div_s(a, b)
  if b == 0.0 then trap_div0() end
  local sa = a; if sa >= 2147483648.0 then sa = sa - 4294967296.0 end
  local sb = b; if sb >= 2147483648.0 then sb = sb - 4294967296.0 end
  -- The one case with no representable answer: -2^31 / -1 is +2^31.
  if sa == -2147483648.0 and sb == -1.0 then trap_overflow() end
  local na = sa < 0.0; local ua = na and -sa or sa
  local nb = sb < 0.0; local ub = nb and -sb or sb
  local q = (ua - ua % ub) / ub
  if na ~= nb then q = -q end
  return q % 4294967296.0
end

local function rem_s(a, b)
  if b == 0.0 then trap_div0() end
  local sa = a; if sa >= 2147483648.0 then sa = sa - 4294967296.0 end
  local sb = b; if sb >= 2147483648.0 then sb = sb - 4294967296.0 end
  local na = sa < 0.0; local ua = na and -sa or sa
  local nb = sb < 0.0; local ub = nb and -sb or sb
  -- rem_s does NOT trap on -2^31 % -1; the answer is 0, which falls out here.
  local r = ua % ub
  if na then r = -r end
  return r % 4294967296.0
end

local function div_u(a, b)
  if b == 0.0 then trap_div0() end
  return (a - a % b) / b
end

local function rem_u(a, b)
  if b == 0.0 then trap_div0() end
  return a % b
end

-- i32.clz. frexp is pure exponent extraction: for a > 0 it returns m, e with
-- a = m * 2^e and 0.5 <= m < 1, so e is exactly floor(log2(a)) + 1.
local function clz32(a)
  if a == 0.0 then return 32.0 end
  local _, e = frexp(a)
  return 32.0 - e
end

-- i32.ctz by binary search on divisibility. Five predicted branches and no C
-- call, which beats the band(a, -a) trick that would need bit32.
local function ctz32(a)
  if a == 0.0 then return 32.0 end
  local n = 0
  if a % 65536.0 == 0.0 then a = a / 65536.0; n = 16 end
  if a % 256.0 == 0.0 then a = a / 256.0; n = n + 8 end
  if a % 16.0 == 0.0 then a = a / 16.0; n = n + 4 end
  if a % 4.0 == 0.0 then a = a / 4.0; n = n + 2 end
  if a % 2.0 == 0.0 then n = n + 1 end
  return n
end

local function popcnt32(a)
  local n = PC[a % 256.0]
  a = (a - a % 256.0) / 256.0
  n = n + PC[a % 256.0]
  a = (a - a % 256.0) / 256.0
  n = n + PC[a % 256.0]
  a = (a - a % 256.0) / 256.0
  return n + PC[a]
end

-- Variable-distance shifts. Constant distances are folded by the emitter and
-- never reach these.
local function shl32(a, b)
  local n = b % 32.0
  if n == 0.0 then return a end
  return (a % P2[32 - n]) * P2[n]
end

local function shr_u32(a, b)
  local n = b % 32.0
  if n == 0.0 then return a end
  local d = P2[n]
  return (a - a % d) / d
end

local function shr_s32(a, b)
  local n = b % 32.0
  if n == 0.0 then return a end
  local t = a; if t >= 2147483648.0 then t = t - 4294967296.0 end
  local d = P2[n]
  -- Lua's % is floored, so t - t % d is a multiple of d at or below t; the
  -- quotient is therefore floor(t / d), which is what an arithmetic shift means.
  return ((t - t % d) / d) % 4294967296.0
end

-- Rotates. The two halves occupy disjoint bit ranges, so they combine with +
-- and never need bor.
local function rotl32(a, b)
  local n = b % 32.0
  if n == 0.0 then return a end
  local lo = P2[32 - n]
  return (a % lo) * P2[n] + (a - a % lo) / lo
end

local function rotr32(a, b)
  local n = b % 32.0
  if n == 0.0 then return a end
  local d = P2[n]
  return (a % d) * P2[32 - n] + (a - a % d) / d
end


-- ---------------------------------------------------------------------------
-- Linear memory: A VECTOR OF SHARDS, ALWAYS.
--
-- `mem` here -- and MEM in a generated chunk -- is the shard VECTOR: a plain
-- 1-based array whose entry s+1 is shard s, itself a 1-based Lua table of u32
-- words. Words rather than bytes because guest output is dominated by
-- i32/i64/f64 access, and byte offsets inside aligned structs are statically
-- known, so a byte read folds to one lookup plus a mask.
--
-- A SHARD IS 2^19 WORDS = 2^21 BYTES = 2 MiB, and that is not a tuning knob.
-- Above 2^20 keys a Factorio Lua table stops behaving like an array FOR ALL of
-- its keys: every load and store gets 20-40x slower, permanently, and every
-- LOAD pays seconds rebuilding it. 2^19 is exactly half that wall, so a shard
-- can never stop being an array however the memory grows. 2^21 bytes also makes
-- the shard select two opcodes -- `a % 2097152` and a subtraction -- where
-- `bit32.rshift`/`band` would be two C calls and measured 1.5x worse. See
-- agents/sharding.md.
--
-- THE CONSTANTS ARE WRITTEN AS LITERALS, NOT AS NAMES, AND THAT IS DELIBERATE.
-- 2097152 (bytes per shard) and 524288 (words per shard) appear inline in every
-- function below. A `local SHB = 2097152` at column zero would be TWO more of
-- Lua's 200 chunk locals taken from every guest's budget for globals and
-- promoted upvalues, forever, to save nothing at runtime: a numeral is already
-- in the constant table. TestPromotionLeavesTheMarginItPromises is what notices
-- when that budget moves.
--
-- The shard of byte address `a` is `(a - a % 2097152) / 2097152 + 1` and the
-- offset within it is `a % 2097152`. Below 2 MiB every address is in shard 1
-- and the generated fast path never computes either -- see the SHBOUND merge in
-- agents/sharding.md section 5.
--
-- Addresses are byte addresses. wasm adds the static offset in INFINITE
-- precision and traps rather than wrapping, so the sum never needs masking --
-- but it does need a bounds check, since it can legitimately exceed 2^32.
--
-- align= in a memarg is a HINT, not a guarantee: unaligned access is legal and
-- must give the right answer, so the aligned path is a speculation, never a
-- substitute for the general one.
--
-- WHAT CAN STRADDLE A SHARD BOUNDARY, AND WHAT CANNOT. A shard is a whole
-- number of words, so a 4-BYTE ALIGNED access never crosses one: its word is
-- wholly inside a shard by construction. An 8-byte aligned access CAN -- at
-- offset 2097148 its low word is the last of one shard and its high word is the
-- first of the next -- and that is the one access shape with no analogue in the
-- flat representation. It is handled explicitly in st64/ld_f64/xld_f64 below,
-- and the ORDER matters: the bounds check for the whole 8 bytes comes first, so
-- an out-of-range store leaves BOTH shards untouched as the spec requires. An
-- unaligned access of any width straddles freely and goes byte-wise, where each
-- byte selects its own shard.
-- ---------------------------------------------------------------------------

local function trap_oob() error(TRAPS.oob, 0) end
local function trap_indirect() error(TRAPS.indirect, 0) end
local function trap_uninit() error(TRAPS.uninit, 0) end

-- THE TWO BYTE LEAVES, AND WHY THEY CARRY THE SHARD-0 FAST PATH TOO.
--
-- The shard select is written out rather than factored into a helper: a call
-- would double the cost of the very path these exist to make cheap, and these
-- two are the leaves every byte-wise access funnels through -- ld8/ld16/st8b/
-- st16, the head and tail of every bulk operation, fk_str, fk_wstr, and the
-- emitter's data-segment initialisation.
--
-- The unconditional form -- `o = a % 2097152` then two divisions -- costs five
-- more opcodes than the flat version on a function that only had about twelve,
-- and BYTE STORES ARE NOT INLINED AT ANY OPT LEVEL (agents/optimizer.md: a
-- store's read-modify-write needs five values against two scratch registers),
-- so that cost lands on every byte a guest writes. Measured on `real_names`,
-- the string kernel, it was 1.07x on TinyGo and 1.10x on Rust -- outside the
-- run-to-run spread and the only below-wall regression the corpus showed.
--
-- The branch below is the same merge the emitter makes: inside the first shard
-- the body is the flat one plus one compare and one `mem[1]`, which is the
-- floor for any two-level structure. Above it, the full select. `a < 2097152`
-- is sufficient for a single byte with no further condition -- a byte is inside
-- whichever shard its address names.
local function ld8raw(mem, a)
  local p = a % 4
  local i = (a - p) / 4 + 1
  local t = mem[1]
  if a >= 2097152 then
    local o = a % 2097152
    t = mem[(a - o) / 2097152 + 1]
    i = (o - p) / 4 + 1
  end
  local w = t[i]
  local d = P2[8 * p]
  return ((w - w % d) / d) % 256
end

local function st8raw(mem, a, v)
  local p = a % 4
  local i = (a - p) / 4 + 1
  local t = mem[1]
  if a >= 2097152 then
    local o = a % 2097152
    t = mem[(a - o) / 2097152 + 1]
    i = (o - p) / 4 + 1
  end
  local d = P2[8 * p]
  local w = t[i]
  local hi = w - w % (d * 256)
  t[i] = hi + (v % 256) * d + w % d
end

-- Expanded for the same reason st8b is, and at no cost in names: `size` picks
-- the shard in one compare and the call to ld8raw goes with it. This is the
-- path a byte load takes below -opt=3 and through the host memio surface; at
-- -opt=3 the emitter inlines its own.
local function ld8(mem, size, a)
  if a < 0 or a + 1 > size then trap_oob() end
  local t, o = mem[1], a
  if size > 2097152 then
    o = a % 2097152
    t = mem[(a - o) / 2097152 + 1]
  end
  local p = o % 4
  local w = t[(o - p) / 4 + 1]
  local d = P2[8 * p]
  return ((w - w % d) / d) % 256
end

local function ld16(mem, size, a)
  if a < 0 or a + 2 > size then trap_oob() end
  return ld8raw(mem, a) + ld8raw(mem, a + 1) * 256
end

local function ld32(mem, size, a)
  if a < 0 or a + 4 > size then trap_oob() end
  if a % 4 == 0 then
    -- Same merge as the byte leaves and as the emitted access: a 4-byte
    -- aligned address below 2097152 has its whole word inside shard 0, since a
    -- shard is 524,288 WHOLE words.
    if a < 2097152 then return mem[1][a / 4 + 1] end
    local o = a % 2097152
    return mem[(a - o) / 2097152 + 1][o / 4 + 1]
  end
  return ld8raw(mem, a) + ld8raw(mem, a + 1) * 256
       + ld8raw(mem, a + 2) * 65536 + ld8raw(mem, a + 3) * 16777216
end

-- Dirty-page SET for --persist=packed.
--
-- What is recorded is the set of 4 KiB pages written since the last flush.
-- It used to be a min/max byte RANGE, on the grounds that a page set costs a
-- division and a table write on EVERY store where a min/max costs two compares.
-- That reasoning was right about the cost and wrong about what it was buying:
-- the cost of a flush is the SPAN, and one host call that touches a static near
-- address zero and a heap object near the top of a 1 MiB memory repacks every
-- page in between. The first real mod measured a 200-rig build at 447 s in
-- packed mode against ~15 s in table mode, and shipped table -- whose giant word
-- table then gave Lua's incremental GC a 27.8 ms worst tick on an IDLE map.
-- Packed's page count is what stood between that mod and the mode with the small
-- save and the small GC surface.
--
-- DPLO/DPHI ARE NOT THE RANGE. They are the byte bounds of the ONE page most
-- recently marked, and they exist so that the common case costs exactly what the
-- min/max cost: two compares. A store whose whole span lies inside the cached
-- page has nothing to do, because that page is already in the set -- which is
-- every store in a sequential run after the first, and every store in a burst
-- that stays inside 4 KiB. Only a store that leaves the cached page calls
-- `MEMPACK.mark`, which does the division, adds each page of its span to the set,
-- and re-caches. So the division-per-store the original comment feared is a
-- division per PAGE CHANGE.
--
-- The set itself, and the function that adds to it, live on MEMPACK -- whose
-- declaration is hoisted here so the store leaves below can see it. Chunk
-- locals are the scarcest thing a generated chunk has and the min/max pair spent
-- two, so `MEMPACK.mark` rather than a `local memdirty` is what keeps this
-- change net-zero against Lua's 200. It is not free: a mark is an upvalue read
-- plus a table index rather than an upvalue read. It is paid only when a store
-- LEAVES the cached page, which is the same event that pays for the division.
-- TestPromotionLeavesTheMarginItPromises is what says this is not a style
-- preference -- one more chunk-scope name and a guest with 32 globals stops
-- compiling at -opt=3 while still compiling at -opt=2.
--
-- The full set of writers, because a shorter list here was wrong for two
-- milestones. The EMITTER's stores funnel into three leaves -- st8b, st16, st32
-- (st64 marks its own range on the aligned path and delegates to two st32s
-- otherwise; st_f64 and xst_f64 are st64s, xst_f32 is an st32). Three more
-- write the word table directly and mark their whole span in one update rather
-- than per byte, which is most of why they are fast: mem_copy, mem_fill and
-- fk_wstr.
--
-- fk_wstr is the one that does not come to mind, and it went unmarked until it
-- was caught. It is the HOST's way in rather than the guest's -- the path every
-- string the ABI marshals back takes -- and it writes through st8raw and the
-- word table directly, so it marks its own range or nothing does. Bytes written
-- through it were live in the word table and missing from every flush after
-- them, so they disappeared one save/load cycle later, a long way from the call
-- that wrote them.
--
-- The rule that leaves: write `mem` without going through st8b, st16 or st32
-- and you mark your own span here. Two writers are exempt, both because the
-- span would be meaningless rather than because they are special cases worth
-- copying: `grow` appends zeros, and an absent page already restores as zeros;
-- MEMPACK.restore installs the saved image wholesale and clears the set itself.
--
-- WHICH IS WHY --persist=packed REFUSES THE INLINED 8-BYTE STORE. At -opt=3 the
-- emitter otherwise expands the aligned path of an i64/f64 store at the use
-- site and writes MEM directly -- past every writer above. The bytes land in
-- the live table, the flush never learns the page changed, and the value is
-- silently absent from the save, one load cycle away from the code that wrote
-- it. `builder.inlineWideStores` is gated on the mode for exactly this, and
-- TestAnEightByteStoreInPackedModeReachesTheSave fails when the gate is
-- removed. The inlined LOADS are unaffected: nothing records a read.
--
-- THE PAGE SET DOES NOT CHANGE THAT DECISION, and it is worth saying why since
-- it changes the shape of the obligation. Marking is now one call behind a
-- two-compare test rather than four inline compares, so the 8-byte store could
-- carry it in one line exactly as the 4-byte one does -- the mechanism is no
-- longer the objection. What has not changed is that the objection was never
-- mechanical: a second copy of the marking rule in generated code is a second
-- place to forget it, and the 4-byte store carries one only because it is the
-- store that dominates real guests. Enabling the wide one is a size-and-speed
-- question with its own measurement, not a consequence of this change.
--
-- The inlined 4-byte store takes the other route: it emits this same update
-- inline and so stays correct in every mode. That asymmetry is deliberate but
-- not principled -- see TestTheInlinedStoreStillDirtiesItsPage.
--
-- MEMDIRTY gates it so table and none mode pay one upvalue read and one test.
-- The flag is set by the generated chunk only when the module was compiled
-- --persist=packed.
local MEMDIRTY = false
local DPLO, DPHI = math.huge, -1
-- Declared here rather than at its `do` block below, so that the store leaves
-- can name it. The block fills it in; nothing calls MEMPACK.mark before then,
-- because nothing arms MEMDIRTY before then either.
local MEMPACK = {}

-- THE BYTE STORE IS EXPANDED HERE RATHER THAN DELEGATING TO st8raw, and that
-- is the one place in this file where a body is deliberately written twice.
--
-- A byte store is not inlined at ANY opt level -- its read-modify-write needs
-- the byte's position, the word index, a power-of-two divisor and the old word
-- live at once, five values against a function's two scratch registers -- so
-- every byte a guest writes goes through this call, and st8raw's on top of it.
-- Instrumented on `real_names`, the string kernel: 2,177,780 st8b calls in one
-- rep and no mem_copy or mem_fill at all. It was the whole of that kernel's
-- 1.13x, and the only below-wall regression left in the corpus.
--
-- Two things happen here. `size <= 2097152` picks the shard with ONE compare
-- and no address arithmetic -- the bounds check above already proved
-- `a + 1 <= size`, so below 2 MiB the whole memory IS shard 0, the same merge
-- the emitted access makes against SHBOUND. And expanding st8raw's body
-- removes a CALL that the flat version also paid, which more than covers the
-- compare and the `mem[1]`. Measured: `real_names` 1.13x -> 1.00x, and st8b
-- itself slightly FASTER than the flat form.
--
-- st8raw stays exactly as it is, vector-taking and with its own address-based
-- fast path, because the emitter's data-segment initialisation and the ragged
-- ends of the bulk operations call it with an absolute address and no size.
local function st8b(mem, size, a, v)
  if a < 0 or a + 1 > size then trap_oob() end
  if MEMDIRTY and (a < DPLO or a > DPHI) then MEMPACK.mark(a, a) end
  local t, o = mem[1], a
  if size > 2097152 then
    o = a % 2097152
    t = mem[(a - o) / 2097152 + 1]
  end
  local p = o % 4
  local i = (o - p) / 4 + 1
  local d = P2[8 * p]
  local w = t[i]
  t[i] = w - w % (d * 256) + (v % 256) * d + w % d
end

local function st16(mem, size, a, v)
  if a < 0 or a + 2 > size then trap_oob() end
  if MEMDIRTY and (a < DPLO or a + 1 > DPHI) then MEMPACK.mark(a, a + 1) end
  v = v % 65536
  st8raw(mem, a, v % 256)
  st8raw(mem, a + 1, (v - v % 256) / 256)
end


local function st32(mem, size, a, v)
  if a < 0 or a + 4 > size then trap_oob() end
  if MEMDIRTY and (a < DPLO or a + 3 > DPHI) then MEMPACK.mark(a, a + 3) end
  v = v % 4294967296.0
  if a % 4 == 0 then
    if a < 2097152 then mem[1][a / 4 + 1] = v return end
    local o = a % 2097152
    mem[(a - o) / 2097152 + 1][o / 4 + 1] = v
    return
  end
  st8raw(mem, a, v % 256)
  local r = (v - v % 256) / 256
  st8raw(mem, a + 1, r % 256)
  r = (r - r % 256) / 256
  st8raw(mem, a + 2, r % 256)
  st8raw(mem, a + 3, (r - r % 256) / 256)
end

-- An 8-byte store is ONE access and has to be bounds-checked as one.
--
-- The spec requires an out-of-range store to leave memory untouched. Two
-- independent st32 calls do not: for an address 4 bytes below the end, the low
-- word lands and only the high word traps, so the guest observes a half-written
-- value after a trap it was told changed nothing. Every 8-byte store -- i64 and
-- f64 alike -- goes through here.
-- The aligned path writes both words HERE rather than delegating. Two st32
-- calls would re-do the bounds check this function just did, twice, and re-do
-- the alignment test and the page mark -- for a pair of adjacent words
-- whose indices are i and i+1.
--
-- THE STRADDLE, AND WHY THE ORDER IS THE WHOLE ARGUMENT. Under sharding those
-- two adjacent words are in two different TABLES when the access sits at offset
-- 2097148 of a shard -- the only aligned offset where that happens, since a
-- shard is 524288 whole words. A flat memory has no analogue: there, one bounds
-- check and two writes into one table cannot half-succeed.
--
-- The spec requires an out-of-range store to leave memory UNTOUCHED, so the
-- straddle must not be allowed to write the low word and then discover the high
-- word is out of range. It cannot: the bounds check above covers all EIGHT
-- bytes and runs before either write, so by the time the shard tables are
-- indexed both words are known to exist. `mem[s + 2]` is therefore never nil on
-- this path -- if byte a+7 is in range its shard was built by mem_grow.
--
-- Pinned by TestAStraddlingEightByteAccessCrossesTheShardBoundary, which reads
-- the two halves back as separate 4-byte loads on either side of the boundary,
-- and by TestATrappingStraddleTrapsAndLeavesMemoryUntouched, which grows a
-- guest to a memory that ENDS on a shard boundary so that the last straddle
-- point is also out of range -- the case where writing the low word first would
-- half-write memory and then raise a Lua nil-index error instead of a trap.
local function st64(mem, size, a, lo, hi)
  if a < 0 or a + 8 > size then trap_oob() end
  if a % 4 == 0 then
    if MEMDIRTY and (a < DPLO or a + 7 > DPHI) then MEMPACK.mark(a, a + 7) end
    local o = a % 2097152
    local s = (a - o) / 2097152 + 1
    local i = o / 4 + 1
    lo = lo % 4294967296.0
    hi = hi % 4294967296.0
    if o <= 2097144 then
      local t = mem[s]
      t[i] = lo
      t[i + 1] = hi
    else
      mem[s][i] = lo
      mem[s + 1][1] = hi
    end
    return
  end
  st32(mem, size, a, lo)
  st32(mem, size, a + 4, hi)
end

-- memory.grow returns the PREVIOUS size in pages, or -1 (as an unsigned i32)
-- when the request cannot be satisfied. Growing is a one-time cost paid in
-- table appends; prefer a generous initial size over growing during gameplay.
-- Read n bytes of guest memory as a Lua string.
--
-- This is how anything crosses out of the guest: wasm has no string type, so a
-- guest hands the host a (pointer, length) pair and the host reassembles it.
-- table.concat over a byte array rather than repeated `..`, because Lua strings
-- are immutable and interned -- concatenating in a loop is quadratic and leaves
-- every intermediate in the string table for the collector to sweep, inside a
-- lockstep game loop.
--
-- Byte-at-a-time is the honest cost here: message-sized reads are what this is
-- for. Bulk transfer wants a different shape and arrives with the host ABI.
-- ---------------------------------------------------------------------------
-- --persist=packed: linear memory as `string.pack` pages.
--
-- In table mode `storage.fk_mem` IS the word table, which costs nothing while
-- the game runs but puts one `storage` entry per word into every save and every
-- multiplayer join. Packed mode keeps the live table outside `storage` and
-- mirrors it into strings, which serialize and ship far better -- at the cost of
-- repacking whatever changed after each guest call.
--
-- PAGE SIZE IS 4 KiB, NOT THE 64 KiB THE PLAN ASSUMED. Repacking is per page, so
-- the page size is the granularity of the incremental cost: a guest that writes
-- one word makes a whole page dirty. 64 KiB pages would mean 16,384 words
-- repacked for one store. 4 KiB costs 1,024, and the extra `storage` entries are
-- strings -- 256 of them for a 1 MiB heap against the 262,144 numbers table mode
-- would have stored.
--
-- Everything lives in this one table so the whole feature costs the chunk a
-- SINGLE local. Chunk locals are the scarcest resource a generated chunk has:
-- the prelude already takes most of Lua's 200 and every guest global competes
-- for what is left. MEMPACK itself is declared up beside MEMDIRTY, because the
-- store leaves call MEMPACK.mark and are written above this block; only the
-- body is here.
do
  local PAGEW = 1024                        -- words per page
  local PAGEB = PAGEW * 4                   -- bytes per page
  local CHUNKW = 256                        -- words per string.pack call
  -- Packed in runs of 256 rather than 1,024 at once: string.pack takes one
  -- argument per value and table.unpack pushes them all onto the C stack, which
  -- has a hard limit (LUAI_MAXCSTACK). 256 is comfortably inside it.
  local FMT = string.rep("<I4", CHUNKW)

  -- THE DIRTY-PAGE SET, in two halves that answer two different questions.
  --
  -- DPG is the set -- `DPG[p]` is true for a page written since the last flush --
  -- and it is what makes marking idempotent, so a hot loop writing one page a
  -- million times adds one entry.
  --
  -- DPQ is the same pages as a LIST, in the order they were first touched, and
  -- it exists for determinism rather than for speed. Iterating the set with
  -- `pairs` would repack exactly the same pages with exactly the same bytes, so
  -- the flush's RESULT is order-independent -- but the order in which keys are
  -- inserted into a Lua table is part of how that table is laid out, and what
  -- lands in `storage` is a saved, CRC'd, multiplayer-synchronised structure.
  -- This ABI does not bet a desync on a serialiser's iteration order when
  -- appending to an array costs one table store per newly dirtied page. It also
  -- makes the reset free: DPN = 0 empties the list, and the set is cleared as
  -- the flush walks it.
  local DPG, DPQ, DPN = {}, {}, 0

  -- THE SET HAS TWO CONSUMERS SINCE --gc=collected, and they clear at different
  -- points, which is the whole reason this second pair exists.
  --
  -- --persist=packed drains the set after EVERY guest call: a flush repacks the
  -- pages and forgets them. The collector drains it once per collection STEP,
  -- which is once a tick at most and only while a collection is marking. Run
  -- both against one set and packed wins every race -- the collector finds the
  -- set empty and loses every write made since its last step, which is not
  -- stale memory after a reload (packed's failure) but a live object the sweep
  -- reclaims.
  --
  -- So a flush, while the collector is armed, MOVES the pages it is about to
  -- forget onto GCQ instead of dropping them. That is one table store per dirty
  -- page per call, paid only during a collection, and nothing at all on the
  -- store path -- which is the half that had to stay free.
  --
  -- PKARM and GCARM are which consumer asked for the marking. MEMDIRTY is their
  -- OR, and gc_disarm restores it to PKARM rather than to false, because a
  -- packed guest that also collects must not come out of a collection with its
  -- save tracking turned off.
  local GCG, GCQ, GCN = {}, {}, 0
  local PKARM, GCARM = false, false

  -- Assigns the forward-declared chunk local above; every store leaf calls this.
  --
  -- The floor is `a - a % PAGEB` rather than math.floor(a / PAGEB): arithmetic
  -- only, no call, and the address is already a non-negative integral double by
  -- Invariant A. Marking is by SPAN because a single access can straddle a page
  -- boundary -- an unaligned 4-byte store, and every one of mem_copy, mem_fill
  -- and fk_wstr, which mark a whole run in one call.
  --
  -- The cached page is the span's LAST page, which is the right guess: writes
  -- run upward far more often than downward, so the next store is most likely
  -- to land there. A downward run simply misses the cache each time and pays the
  -- call -- correct, and no worse than the byte range was.
  MEMPACK.mark = function(a, b)
    local p = (a - a % PAGEB) / PAGEB
    local q = (b - b % PAGEB) / PAGEB
    for i = p, q do
      if not DPG[i] then
        DPG[i] = true
        DPN = DPN + 1
        DPQ[DPN] = i
      end
    end
    DPLO = q * PAGEB
    DPHI = DPLO + PAGEB - 1
  end

  -- Everything is clean again: forget the set AND the cached page, which would
  -- otherwise claim a page is dirty when it is not, and swallow the next store.
  local function dirty_clear()
    for i = 1, DPN do DPG[DPQ[i]] = nil end
    DPN = 0
    DPLO, DPHI = math.huge, -1
  end

  -- THE PAGE SET SURVIVES SHARDING UNCHANGED, AND THE ARITHMETIC IS WHY.
  --
  -- A page is 4 KiB and a shard is 2 MiB; both are powers of two and the page
  -- is the smaller, so A PAGE CAN NEVER STRADDLE A SHARD BOUNDARY -- there are
  -- exactly 512 pages per shard, always aligned. The set still indexes by BYTE
  -- address, DPLO/DPHI are still byte bounds, MEMPACK.mark is untouched, every
  -- writer's mark call is unchanged, and the two-compare fast path is unchanged.
  -- The set is also the collector's write barrier and that consumer is
  -- unaffected for the same reason.
  --
  -- ONLY THESE TWO TRANSLATE: page p lives in shard `p >> 9` at word offset
  -- `(p & 511) * PAGEW`. Written as arithmetic rather than bit32 for the reason
  -- the whole file uses arithmetic. Pinned by
  -- TestAPageNeverStraddlesAShardBoundary.
  local function pack_page(mem, p)
    local q = p % 512
    local t = mem[(p - q) / 512 + 1]
    if t == nil then return string.rep("\0", PAGEB) end
    local w0 = q * PAGEW
    local chunks, acc = {}, {}
    for c = 0, PAGEW / CHUNKW - 1 do
      local o = w0 + c * CHUNKW
      -- `or 0` matters: a page past what the guest has touched has nil words,
      -- and string.pack would raise on one.
      for k = 1, CHUNKW do acc[k] = t[o + k] or 0 end
      chunks[c + 1] = string.pack(FMT, table.unpack(acc))
    end
    return table.concat(chunks)
  end

  -- Creates the shard when it is missing, because RESTORE IS WHERE A SAVED SIZE
  -- BECOMES SHARDS. A load rebuilds the live memory at the module's DECLARED
  -- size, so a guest that grew past a shard boundary before the save comes back
  -- to a vector with fewer shards than the save has pages for. restore is
  -- driven by the saved SIZE, so walking it is exactly the right place to
  -- materialise them.
  local function shard_for(mem, p)
    local q = p % 512
    local s = (p - q) / 512 + 1
    local t = mem[s]
    if t == nil then t = {} mem[s] = t end
    return t, q * PAGEW
  end

  local function unpack_page(mem, p, s)
    local t, w0 = shard_for(mem, p)
    local pos = 1
    for c = 0, PAGEW / CHUNKW - 1 do
      local o = w0 + c * CHUNKW
      local u = { string.unpack(FMT, s, pos) }
      pos = u[CHUNKW + 1]                   -- unpack returns the next position last
      for k = 1, CHUNKW do t[o + k] = u[k] end
    end
  end

  -- Turn tracking on. Called by the generated chunk only in packed mode.
  MEMPACK.arm = function()
    PKARM = true
    MEMDIRTY = true
    dirty_clear()
  end

  -- ---------------------------------------------------------------------------
  -- The collector's half of the same mechanism.
  --
  -- agents/gc.md stage A: "Arm MEMDIRTY while a collection is marking. Disarm it
  -- when the collection finishes. That is the barrier." Everything below is the
  -- plumbing that makes the page set readable from the guest; the barrier itself
  -- is the flag, and it was already there.
  --
  -- Armed ONLY while marking. A sweep needs no barrier -- the mark bitmap is
  -- fixed once marking terminates, so nothing a store does can change a decision
  -- the sweep makes -- which is why the expensive phase is also the free one to
  -- incrementalize, and why an armed guest pays the 7-13% store cost for the
  -- mark phase only rather than for the whole cycle.
  -- ---------------------------------------------------------------------------

  -- No dirty_clear here, deliberately: in packed mode the set may hold pages the
  -- next flush still owes the save, and arming the collector must not eat them.
  -- Over-recording is free (a page re-scanned for nothing); under-recording is a
  -- use-after-free.
  MEMPACK.gc_arm = function()
    GCARM = true
    MEMDIRTY = true
  end

  MEMPACK.gc_disarm = function()
    GCARM = false
    MEMDIRTY = PKARM
    GCN = 0
    for p in pairs(GCG) do GCG[p] = nil end
  end

  -- Hand the collector the pages written since its last step, as i32 words in
  -- guest memory at `base`. Returns how many, or 4294967295 when there were more
  -- than `cap` -- which the guest reads as "assume everything is dirty" and
  -- recovers from with one budgeted full re-scan. Overflow is a performance
  -- event here exactly as gray-stack overflow is inside the collector.
  --
  -- WHO OWNS DPQ depends on the mode and getting it wrong either drops writes or
  -- repacks the world. In packed mode the flush owns it and has already moved
  -- what it cleared onto GCQ, so this must not touch it. In table and none mode
  -- nothing else looks at it, so this is its only drain point.
  --
  -- The write is marked like any other write to guest memory -- the rule this
  -- whole block exists to enforce -- which puts the buffer's own page into the
  -- set. That page is the guest's .bss, below the heap, and the collector drops
  -- a dirty record below heapBase because the statics are re-scanned wholesale
  -- as roots at every termination attempt anyway. So it costs one entry per
  -- step, forever, and is filtered on the other side.
  MEMPACK.gc_drain = function(mem, size, base, cap)
    if not PKARM then
      for i = 1, DPN do
        local p = DPQ[i]
        DPG[p] = nil
        if not GCG[p] then
          GCG[p] = true
          GCN = GCN + 1
          GCQ[GCN] = p
        end
      end
      DPN = 0
      DPLO, DPHI = math.huge, -1
    end
    local n = GCN
    for p in pairs(GCG) do GCG[p] = nil end
    GCN = 0
    if n == 0 then return 0 end
    if n > cap or base + n * 4 > size then return 4294967295 end
    -- Written through the word table directly, in DPQ order, because insertion
    -- order is what the collector re-scans in and what lands in `storage` is
    -- CRC'd and multiplayer-synchronised. pairs(GCG) would be a per-client
    -- ordering of the collector's work; the answer would be the same and the
    -- free-list layout that came out of it would not.
    -- Shard-selected per word rather than split into pieces: the buffer is a
    -- few hundred words at most and this runs once per collection STEP, so the
    -- two extra opcodes per word are invisible beside the loop that follows it
    -- in the guest.
    local w = base / 4
    for i = 1, n do
      local o = (w + i - 1) % 524288
      mem[(w + i - 1 - o) / 524288 + 1][o + 1] = GCQ[i]
    end
    MEMPACK.mark(base, base + n * 4 - 1)
    return n
  end

  MEMPACK.pages = function(size) return math.ceil(size / PAGEB) end

  -- Every page, for a fresh save.
  MEMPACK.all = function(mem, size)
    local out = {}
    for p = 0, MEMPACK.pages(size) - 1 do out[p + 1] = pack_page(mem, p) end
    dirty_clear()
    return out
  end

  -- Only the pages actually written. Returns how many were rewritten, which is
  -- what a cost measurement needs.
  --
  -- The `p <= last` test is what a byte range used to get by clamping: a page
  -- can be marked and then fall outside the memory, because `size` here is read
  -- after the call and a guest can shrink nothing but the caller can hand a
  -- smaller one. Skipping it rather than clamping is the difference between a
  -- set and a range -- there is no "everything between" to truncate.
  MEMPACK.flush = function(mem, size, out)
    if DPN == 0 then return 0 end
    local last = MEMPACK.pages(size) - 1
    local n = 0
    for i = 1, DPN do
      local p = DPQ[i]
      DPG[p] = nil
      -- The dual consumer. A page this flush forgets is a page the collector
      -- has not been told about yet, so while a collection is marking it moves
      -- onto GCQ instead of being dropped. Nothing here is on the store path;
      -- it is one table store per dirty page per call, during a collection.
      if GCARM and not GCG[p] then
        GCG[p] = true
        GCN = GCN + 1
        GCQ[GCN] = p
      end
      if p <= last then
        out[p + 1] = pack_page(mem, p)
        n = n + 1
      end
    end
    DPN = 0
    DPLO, DPHI = math.huge, -1
    return n
  end

  -- Lay a saved page array back over the live word table.
  --
  -- Driven by the SAVED SIZE rather than by the array, and that is the whole
  -- correctness argument. flush writes only the pages the set says were
  -- touched, so a guest that grew its heap and wrote one word leaves an array
  -- with a HOLE in it -- and `#` on a table with a hole is allowed to stop at
  -- the hole, which silently dropped every page past it, including the only
  -- one the guest had written.
  --
  -- An absent page is not missing data, it is ZEROS. Everything written since
  -- the last full pack is dirty and therefore flushed, so a page nobody wrote
  -- holds nothing. Writing the zeros matters after a grow: the live table was
  -- rebuilt by _initialize at the ORIGINAL size and does not have those words
  -- at all, and a read would find nil rather than trap -- nil arithmetic deep
  -- inside guest code, a long way from the load that caused it.
  MEMPACK.restore = function(mem, pages, size)
    for p = 0, MEMPACK.pages(size) - 1 do
      local s = pages[p + 1]
      if s then
        unpack_page(mem, p, s)
      else
        local t, w0 = shard_for(mem, p)
        for k = 1, PAGEW do t[w0 + k] = 0 end
      end
    end
    dirty_clear()
  end
end

-- ---------------------------------------------------------------------------
-- THE BULK OPERATIONS, AND WHY THEY SHARE ONE BLOCK.
--
-- fk_str, fk_wstr, mem_copy and mem_fill are the four functions that walk a
-- SPAN of guest memory rather than one access, so they are the four that have
-- to split that span at shard boundaries. They now share a `do` block for one
-- reason: the shard helpers they all want -- `shof` and `rdw` below -- are
-- block-scoped there and cost the chunk nothing. At column zero they would be
-- two more of Lua's 200 taken from every guest forever. The four names declared
-- here are exactly the four that were declared before, so this block is
-- net-zero against the chunk-local budget.
--
-- THE SPLIT IS FREE AND THAT IS MEASURED, not assumed. A 1 MiB fill straddling
-- a shard boundary costs 9.4-12.5 ms below the wall against the flat form's
-- 9.5-13.6, and 8.7-13.4 ms above it against 699-2,096 -- 89-159x. The reason
-- is structural: each piece is the SAME plain loop the flat version ran, and a
-- piece boundary is one loop preamble per 2 MiB. See agents/sharding.md.
--
-- Every one of these was verified byte-for-byte against the flat form under
-- bin/lua52f before anything was timed. A fast copy of the wrong words is not a
-- result.
-- ---------------------------------------------------------------------------
local fk_str, fk_wstr, mem_copy, mem_fill
do
  -- string.pack/unpack localised INSIDE the block for the same reason the
  -- shard helpers are: a column-zero local comes straight out of a guest's
  -- 200-local budget, and TestPromotionLeavesTheMarginItPromises notices.
  local pack_ = string.pack
  local unpack_ = string.unpack

  -- The shard table holding byte address `a`, and `a`'s offset within it.
  local function shof(mem, a)
    local o = a % 2097152
    return mem[(a - o) / 2097152 + 1], o
  end

  -- One word by GLOBAL 0-based word index. Used only where a single word is
  -- wanted outside a piece loop -- the copy path's look-ahead seed.
  local function rdw(mem, w)
    local o = w % 524288
    return mem[(w - o) / 524288 + 1][o + 1]
  end

  -- Guest memory into a Lua string.
  --
  -- THE MIRROR OF fk_wstr, AND IT DID NOT GET THE SAME TREATMENT FOR A WHOLE
  -- MILESTONE -- the same asymmetry this project already recorded once, when
  -- the reason sub-word accesses stayed function calls turned out to have been
  -- measured for STORES and inherited by loads. fk_wstr was batched to four
  -- words per string.unpack; this stayed one C call and one table slot per
  -- BYTE.
  --
  -- It matters more than it looks, because this is the whole of the tier-2
  -- decode: ablating read_dyn over a create_entity-shaped map showed a bare
  -- number at 86 ns and a single 14-character string at 1.72 us, so a six-key
  -- map's 14 us is almost entirely its ten strings -- keys included.
  --
  -- string.pack is the exact inverse of the unpack fk_wstr uses: four words in,
  -- sixteen bytes out, one C call, and the byte-to-string arithmetic
  -- disappears into the format string.
  function fk_str(mem, size, ptr, n)
    if ptr < 0 or n < 0 or ptr + n > size then trap_oob() end
    if n == 0 then return "" end

    -- A SHORT STRING TAKES THE OLD PATH, because the word path cannot pay for
    -- its own setup below about eight bytes -- the head and tail eat the whole
    -- string and two loop preambles are added for nothing. Measured: without
    -- this, a map of six one-character keys went 4.15 -> 4.54 us. Tier 2 is
    -- FULL of short strings; they are the keys.
    if n < 8 then
      local out = {}
      for i = 1, n do out[i] = char(ld8raw(mem, ptr + i - 1)) end
      return concat(out)
    end

    local out, k = {}, 0

    -- The ABI's allocator promises no alignment at all, so the head and tail
    -- are per-byte and only the middle is word-wise -- fk_wstr's shape exactly.
    -- The per-byte legs go through ld8raw, which selects its own shard, so only
    -- the word-wise middle needs splitting.
    local head = (4 - ptr % 4) % 4
    if head > n then head = n end
    for i = 1, head do
      k = k + 1
      out[k] = char(ld8raw(mem, ptr + i - 1))
    end

    local words = (n - head - (n - head) % 4) / 4
    local aw = (ptr + head) / 4             -- GLOBAL 0-based word index

    -- One piece per shard the span touches, and inside a piece the quad-batched
    -- loop is exactly what it was: `t` is a plain word table and `i` a plain
    -- index into it.
    local done = 0
    if size <= 2097152 then
      -- One shard, proved by one compare: see mem_copy. The loop below is the
      -- flat one with `mem[1]` bound once.
      local t = mem[1]
      local i, e = aw + 1, aw + 1 + words
      while i + 3 < e do
        k = k + 1
        out[k] = pack_("<I4I4I4I4", t[i], t[i + 1], t[i + 2], t[i + 3])
        i = i + 4
      end
      while i < e do
        k = k + 1
        out[k] = pack_("<I4", t[i])
        i = i + 1
      end
      done = words
    end
    while done < words do
      local off = aw % 524288
      local t = mem[(aw - off) / 524288 + 1]
      local m = 524288 - off
      if m > words - done then m = words - done end
      local i, e = off + 1, off + 1 + m
      while i + 3 < e do
        k = k + 1
        out[k] = pack_("<I4I4I4I4", t[i], t[i + 1], t[i + 2], t[i + 3])
        i = i + 4
      end
      while i < e do
        k = k + 1
        out[k] = pack_("<I4", t[i])
        i = i + 1
      end
      aw, done = aw + m, done + m
    end

    for i = head + words * 4 + 1, n do
      k = k + 1
      out[k] = char(ld8raw(mem, ptr + i - 1))
    end
    return concat(out)
  end

  -- The other direction: a Lua string into guest memory.
  --
  -- The bounds check covers the WHOLE span and happens once. Per-byte checking
  -- would leave a half-written string behind when it tripped, and the spec's
  -- rule for an out-of-range store is that memory is not modified at all -- a
  -- rule st64 already exists to honour for eight bytes, and this honours for n.
  --
  -- The path EVERY string takes into guest memory on the host-call ABI's return
  -- side, so it is worth the same treatment mem_copy and mem_fill got: a
  -- per-byte st8raw is a read-modify-write of a whole word, and the ABI's
  -- allocator hands out addresses with no alignment promise at all.
  --
  -- string.byte takes a RANGE and returns several values, so the four bytes of
  -- a word arrive from one C call rather than four.
  function fk_wstr(mem, size, a, s)
    local n = #s
    if a < 0 or a + n > size then trap_oob() end
    if n == 0 then return end

    -- One page-set update for the whole span, the same shape mem_copy and
    -- mem_fill use. It has to be HERE and not in the loops below: st8raw is a
    -- raw read-modify-write and `t[i] =` is rawer still, so neither the head,
    -- the body nor the tail passes a store leaf that would mark it.
    -- After the n == 0 return, so an empty write never marks [a, a-1].
    --
    -- The page set is unchanged by sharding and indexes BYTE addresses exactly
    -- as before: a page is 4 KiB, a shard is 2 MiB, both powers of two and the
    -- page is smaller, so a page can never straddle a shard boundary.
    if MEMDIRTY and (a < DPLO or a + n - 1 > DPHI) then MEMPACK.mark(a, a + n - 1) end

    local head = (4 - a % 4) % 4
    if head > n then head = n end
    for i = 1, head do st8raw(mem, a + i - 1, byte(s, i)) end

    local words = (n - head - (n - head) % 4) / 4
    local aw = (a + head) / 4               -- GLOBAL 0-based word index

    -- Four words per string.unpack call rather than one word per string.byte.
    -- The batching is the whole win, not unpack itself: one word at a time
    -- through unpack measured identical to the byte form, because either way it
    -- is one C call per word. At four it is one call per four words and the
    -- byte-to-word arithmetic disappears into the format string.
    local pos, done = head + 1, 0
    if size <= 2097152 then
      local t = mem[1]
      local i, e = aw + 1, aw + 1 + words
      while i + 3 < e do
        local x0, x1, x2, x3
        x0, x1, x2, x3, pos = unpack_("<I4I4I4I4", s, pos)
        t[i], t[i + 1], t[i + 2], t[i + 3] = x0, x1, x2, x3
        i = i + 4
      end
      while i < e do
        local x
        x, pos = unpack_("<I4", s, pos)
        t[i] = x
        i = i + 1
      end
      done = words
    end
    while done < words do
      local off = aw % 524288
      local t = mem[(aw - off) / 524288 + 1]
      local m = 524288 - off
      if m > words - done then m = words - done end
      local i, e = off + 1, off + 1 + m
      while i + 3 < e do
        local x0, x1, x2, x3
        x0, x1, x2, x3, pos = unpack_("<I4I4I4I4", s, pos)
        t[i], t[i + 1], t[i + 2], t[i + 3] = x0, x1, x2, x3
        i = i + 4
      end
      while i < e do
        local x
        x, pos = unpack_("<I4", s, pos)
        t[i] = x
        i = i + 1
      end
      aw, done = aw + m, done + m
    end

    for i = head + words * 4 + 1, n do st8raw(mem, a + i - 1, byte(s, i)) end
  end

  -- memory.copy and memory.fill.
  --
  -- These exist as runtime helpers rather than inline loops because that is the
  -- entire performance argument. Binaryen's --llvm-memory-copy-fill-lowering,
  -- which is what a guest gets without them, emits a BYTE-at-a-time wasm loop --
  -- and in a word-table memory a byte store is a read-modify-write of a whole
  -- word plus a page mark. Measured on a 64 KiB aligned copy:
  --
  --   byte loop, compiled     172.80 ns/byte
  --   word loop, compiled      23.82 ns/byte   7.3x
  --   this, word-wise           3.84 ns/byte  45.0x
  --
  -- The win is structural: one bounds check and one page mark for the
  -- whole range instead of one per byte, and four bytes moved per table
  -- operation on the aligned path.
  --
  -- SHARDING SPLITS EACH WORD-WISE LOOP AT SHARD BOUNDARIES, on BOTH streams
  -- independently -- a copy's source and destination are almost never in the
  -- same shard at the same offset. A piece is the largest run that stays inside
  -- one source shard and one destination shard at once, and inside a piece the
  -- loop is byte-for-byte the flat one.
  function mem_copy(mem, size, d, s, n)
    -- Bounds first, and for the WHOLE range: the spec traps before moving any
    -- bytes, so a partially-completed copy is not an allowed outcome.
    if d < 0 or s < 0 or d + n > size or s + n > size then trap_oob() end
    if n == 0 or d == s then return end

    -- One page-set update for the whole range, which is most of why this is
    -- fast: a copy of any length marks its pages once, and a copy that stays
    -- inside the page the last store touched marks nothing at all.
    if MEMDIRTY and (d < DPLO or d + n - 1 > DPHI) then MEMPACK.mark(d, d + n - 1) end

    -- Word-aligned, word-multiple: the fast path, and the one that real memcpy
    -- traffic mostly takes.
    if d % 4 == 0 and s % 4 == 0 and n % 4 == 0 then
      local dw, sw, left = d / 4, s / 4, n / 4
      -- ONE PIECE, WHICH IS ALMOST EVERY COPY, AND THE PIECE MACHINERY IS NOT
      -- FREE. A guest's allocator copies TENS of bytes at a time -- TinyGo's
      -- `append` is the shape that dominates real mod code -- so a copy's fixed
      -- cost matters more than its per-word cost. Measured, 24 bytes: the piece
      -- loop alone was 1.24x the flat form, and `real_names` (the allocating
      -- kernel) was 1.13x end to end because of it.
      --
      -- THREE TESTS, CHEAPEST FIRST, AND THE FIRST ONE IS ONE COMPARE.
      --
      -- `size <= 2097152` proves BOTH streams inside shard 0 without touching
      -- either address, because the bounds check above already proved
      -- `d + n <= size` and `s + n <= size`. It is the same merge the emitted
      -- access makes against SHBOUND, one level up: below 2 MiB the whole
      -- memory IS shard 0, so the flat loops run with `mem[1]` bound once and
      -- nothing else added at all.
      --
      -- That matters more than the per-word cost. A guest's allocator copies
      -- TENS of bytes at a time -- TinyGo's `append` is the shape that
      -- dominates real mod code -- so a copy's FIXED cost is most of it.
      -- Measured, 24 bytes: 1.24x the flat form with only the piece loop,
      -- 1.17x with an address-based shard-0 test, and 1.00x with this one.
      -- `real_names`, the allocating kernel, moved 1.13x -> 1.00x with it.
      --
      -- `db`/`sb` are the 1-based bases, computed ONCE. Writing `dt[doff+1+i]`
      -- in the loop instead is a second ADD per word where the flat form had
      -- one, and that was the 1.14x on a 4 KiB copy.
      if size <= 2097152 then
        local t = mem[1]
        local di, si = dw + 1, sw + 1
        if d < s then
          for i = 0, left - 1 do t[di + i] = t[si + i] end
        else
          for i = left - 1, 0, -1 do t[di + i] = t[si + i] end
        end
        return
      end
      local doff, soff = dw % 524288, sw % 524288
      if 524288 - doff >= left and 524288 - soff >= left then
        local dt = mem[(dw - doff) / 524288 + 1]
        local st = mem[(sw - soff) / 524288 + 1]
        local db, sb = doff + 1, soff + 1
        if d < s then
          for i = 0, left - 1 do dt[db + i] = st[sb + i] end
        else
          for i = left - 1, 0, -1 do dt[db + i] = st[sb + i] end
        end
        return
      end
      if d < s then
        while left > 0 do
          local doff, soff = dw % 524288, sw % 524288
          local dt = mem[(dw - doff) / 524288 + 1]
          local st = mem[(sw - soff) / 524288 + 1]
          local m, m2 = 524288 - doff, 524288 - soff
          if m2 < m then m = m2 end
          if m > left then m = left end
          local db, sb = doff + 1, soff + 1
          for i = 0, m - 1 do dt[db + i] = st[sb + i] end
          dw, sw, left = dw + m, sw + m, left - m
        end
      else
        -- Overlapping downward. memory.copy is memmove, not memcpy: the ranges
        -- may overlap and the result must be as if the source were read first.
        -- The pieces are taken from the TOP for the same reason the words are.
        while left > 0 do
          local de, se = dw + left - 1, sw + left - 1
          local doff, soff = de % 524288, se % 524288
          local dt = mem[(de - doff) / 524288 + 1]
          local st = mem[(se - soff) / 524288 + 1]
          local m, m2 = doff + 1, soff + 1
          if m2 < m then m = m2 end
          if m > left then m = left end
          local db, sb = doff + 1, soff + 1
          for i = 0, m - 1 do dt[db - i] = st[sb - i] end
          left = left - m
        end
      end
      return
    end

    -- Overlapping downward, and only then, is a byte loop the honest answer:
    -- the word path below reads ahead of where it writes. ld8raw/st8raw select
    -- their own shard per byte, so this needs no splitting.
    if d > s and d < s + n then
      for i = n - 1, 0, -1 do st8raw(mem, d + i, ld8raw(mem, s + i)) end
      return
    end

    -- RAGGED, BUT STILL WORD-WISE THROUGH THE MIDDLE.
    --
    -- The naive byte loop here measured ~100 ns/byte, because st8raw is a
    -- read-modify-write of a whole word and ld8raw is a shift and a mask --
    -- about thirteen Lua operations to move one byte. A real TinyGo guest lands
    -- here almost always: its allocator handed out a destination at 1 mod 4
    -- while the source was aligned, so 64 of 66 copies missed the fast path and
    -- bulk-memory measured 3.5x SLOWER than TinyGo's own memmove.
    --
    -- So: byte-copy the head until the DESTINATION is word-aligned, then write
    -- whole destination words, then the tail. When the source is not aligned to
    -- match, each destination word is assembled from two source words with a
    -- shift -- still one table read and one table write per four bytes instead
    -- of two per byte.
    local head = (4 - d % 4) % 4
    if head > n then head = n end
    for i = 0, head - 1 do st8raw(mem, d + i, ld8raw(mem, s + i)) end
    d, s, n = d + head, s + head, n - head

    local words = (n - n % 4) / 4
    if words > 0 then
      local dw = d / 4
      local sh = s % 4
      if sh == 0 then
        local sw, left = s / 4, words
        if size <= 2097152 then
          local t = mem[1]
          local di, si = dw + 1, sw + 1
          for i = 0, left - 1 do t[di + i] = t[si + i] end
          left = 0
        else
          local doff, soff = dw % 524288, sw % 524288
          if 524288 - doff >= left and 524288 - soff >= left then
            local dt = mem[(dw - doff) / 524288 + 1]
            local st = mem[(sw - soff) / 524288 + 1]
            local db, sb = doff + 1, soff + 1
            for i = 0, left - 1 do dt[db + i] = st[sb + i] end
            left = 0
          end
        end
        while left > 0 do
          local doff, soff = dw % 524288, sw % 524288
          local dt = mem[(dw - doff) / 524288 + 1]
          local st = mem[(sw - soff) / 524288 + 1]
          local m, m2 = 524288 - doff, 524288 - soff
          if m2 < m then m = m2 end
          if m > left then m = left end
          local db, sb = doff + 1, soff + 1
          for i = 0, m - 1 do dt[db + i] = st[sb + i] end
          dw, sw, left = dw + m, sw + m, left - m
        end
      else
        -- Little-endian: the destination word is the high 4-sh bytes of one
        -- source word plus the low sh bytes of the next.
        --
        -- The LOOK-AHEAD is what decides where a piece ends. Iteration i reads
        -- source word sw+1+i, so the piece is bounded by the shard of sw+1
        -- rather than of sw -- and `w0`, the previous word, carries across a
        -- piece boundary in the same local it carries across an iteration.
        local sw, left = (s - sh) / 4, words
        local lo = P2[8 * sh]
        local hi = 4294967296.0 / lo
        local w0 = rdw(mem, sw)
        -- The look-ahead reads source word sw+1+i, so the source room is
        -- measured from sw+1 here exactly as it is in the piece loop below.
        if size <= 2097152 then
          local t = mem[1]
          local di, si = dw + 1, sw + 2
          for i = 0, left - 1 do
            local w1 = t[si + i]
            t[di + i] = (w0 - w0 % lo) / lo + (w1 % lo) * hi
            w0 = w1
          end
          left = 0
        else
          local doff, soff = dw % 524288, (sw + 1) % 524288
          if 524288 - doff >= left and 524288 - soff >= left then
            local dt = mem[(dw - doff) / 524288 + 1]
            local st = mem[(sw + 1 - soff) / 524288 + 1]
            local db, sb = doff + 1, soff + 1
            for i = 0, left - 1 do
              local w1 = st[sb + i]
              dt[db + i] = (w0 - w0 % lo) / lo + (w1 % lo) * hi
              w0 = w1
            end
            left = 0
          end
        end
        while left > 0 do
          local doff, soff = dw % 524288, (sw + 1) % 524288
          local dt = mem[(dw - doff) / 524288 + 1]
          local st = mem[(sw + 1 - soff) / 524288 + 1]
          local m, m2 = 524288 - doff, 524288 - soff
          if m2 < m then m = m2 end
          if m > left then m = left end
          local db, sb = doff + 1, soff + 1
          for i = 0, m - 1 do
            local w1 = st[sb + i]
            dt[db + i] = (w0 - w0 % lo) / lo + (w1 % lo) * hi
            w0 = w1
          end
          dw, sw, left = dw + m, sw + m, left - m
        end
      end
    end

    local done = words * 4
    for i = done, n - 1 do st8raw(mem, d + i, ld8raw(mem, s + i)) end
  end

  function mem_fill(mem, size, d, v, n)
    if d < 0 or d + n > size then trap_oob() end
    if n == 0 then return end
    v = v % 256
    if MEMDIRTY and (d < DPLO or d + n - 1 > DPHI) then MEMPACK.mark(d, d + n - 1) end

    -- Head bytes until the destination is word-aligned, then whole words, then
    -- the tail. The alignment gate used to be the WHOLE story and the
    -- else-branch was a byte loop -- which a real TinyGo guest takes almost
    -- always, because its allocator hands out destinations at 1 mod 4. Same
    -- defect the copy path had, and cheaper to fix here: a fill has no source
    -- to shift.
    local head = (4 - d % 4) % 4
    if head > n then head = n end
    for i = 0, head - 1 do st8raw(mem, d + i, v) end
    d, n = d + head, n - head

    local words = (n - n % 4) / 4
    if words > 0 then
      -- One constant, stored whole. The byte is already truncated above.
      local w = v * 16777216 + v * 65536 + v * 256 + v
      local dw, left = d / 4, words
      -- One piece, which is almost every fill, and the same two-test ladder the
      -- copy above uses: shard 0 in two adds and a compare, the general single
      -- shard in a modulo and a division, the piece loop only for a real
      -- crossing.
      if size <= 2097152 then
        local t = mem[1]
        for i = dw + 1, dw + left do t[i] = w end
        left = 0
      else
        local off = dw % 524288
        if 524288 - off >= left then
          local t = mem[(dw - off) / 524288 + 1]
          for i = off + 1, off + left do t[i] = w end
          left = 0
        end
      end
      while left > 0 do
        off = dw % 524288
        local t = mem[(dw - off) / 524288 + 1]
        local m = 524288 - off
        if m > left then m = left end
        for i = off + 1, off + m do t[i] = w end
        dw, left = dw + m, left - m
      end
    end

    for i = words * 4, n - 1 do st8raw(mem, d + i, v) end
  end
end

-- THE ONE-WORD-AT-A-TIME APPEND IS NOT THE DEFECT IT LOOKS LIKE, AND THAT WAS
-- SETTLED BY MEASUREMENT RATHER THAN BY READING IT. (Kept as the record of why
-- the fix is sharding and not a presize; the code below now shards.)
--
-- agents/gc.md recorded a 2.8-SECOND tick for a grow of 222,208 words that
-- crossed the word table's 2^20 entry, against 15 ms for a non-crossing grow of
-- the same size, and attributed it to this loop paying ~19 of Lua's rehashes
-- because every key past the array part lands in the hash part. The NUMBER is
-- real -- reproduced at 2,716 ms in Factorio 2.0.77, and by a bare Lua mod with
-- no guest in it at all, so it is not about wasm or the emitter. The
-- ATTRIBUTION is wrong, and so is the fix it implies.
--
-- What is actually true, measured in game (agents/gc.md, "The 4 MiB wall"):
-- once a Lua table in Factorio holds more than 2^20 keys it stops behaving like
-- an array for ALL of its keys -- 200,000 stores into keys 1..200,000 cost 24 ms
-- at 1,000,000 words and 482 ms at 1,100,000 -- and the 2.7 s is Lua rebuilding
-- the representation, not this loop. So the ORDER the keys arrive in cannot
-- matter, and it does not: ascending (this), descending, top-index-first,
-- 4,096-word chunks, binary-spread and rawset were all measured in game and land
-- within 4% of each other, with binary-spread 2x WORSE. Building the same words
-- from scratch costs the same (2,871 ms), so filling a fresh table and swapping
-- buys nothing either -- and Lua 5.2 has no way to presize an array part past
-- 2^20 in any case: `{table.unpack(t, 1, n)}` is the only constructor form that
-- presizes at all, and it refuses n at 1,000,000 ("too many results to unpack").
--
-- A PRESIZE IS POSSIBLE FOR A SHARD AND IS REFUSED ANYWAY -- the paragraph
-- above is right about the FLAT table and wrong about a shard, and the
-- correction is worth having because "impossible" and "not worth it" send a
-- reader to different places. A shard is 524,288 words, which is UNDER
-- LUAI_MAXSTACK where 2^20 is over it, so `{table.unpack(z, 1, 524288)}`
-- really does clone a zero shard through one C call. Measured in game
-- (scripts/run-growprobe.sh): 34.7-38.7 ms against the fill loop's 56.8-59.2,
-- i.e. 0.59-0.68x. It is refused for two reasons that outrank 40%: it is ONE
-- INDIVISIBLE C CALL, so it cannot be cut into pieces, and the pre-build below
-- makes the same work cost 2-3% more and land on ticks nobody is waiting on --
-- which beats 40% off a stall by the whole size of the stall. It also holds
-- 524,288 stack slots at once, against a limit no FkLua test could see move.
--
-- AND DO NOT TRY TO SEE ANY OF THIS UNDER bin/lua52f. It is stock 5.2.1, whose
-- array part grows to 2^30, and it prices the same crossing at 3.0 ms against
-- 1.3 -- a 2.3x slope where the game has a 27x cliff. The oracle is
-- Factorio-shaped for the SANDBOX, not for table internals.
--
-- WHAT SHARDING CHANGES HERE. The append is still one word at a time and still
-- ascending -- that was never the cost -- but it now walks SHARD BY SHARD, and
-- no shard is ever filled past 2^19 words. Each new shard is created empty and
-- filled sequentially, which is the shape Lua's array part doubles cleanly
-- under, and it stops at half the wall, so the rebuild this comment is about
-- cannot happen at any size. Measured in game: an 8 MiB build in 215 ms against
-- 5,284 flat. There is no crossing tick left to pay.
--
-- THREE RETURNS, NOT TWO. The third is the new SHBOUND -- min(size, 2097152) --
-- because every emitted access opens on it and the chunk has to update it on
-- the same line that updates MEMSIZE. Returning it is what keeps a grow ONE
-- statement in generated code; deriving it at the call site would be a second
-- comparison emitted at every memory.grow.
--
-- ---------------------------------------------------------------------------
-- THE FILL CURSOR, AND WHY A GROW NEED NOT BE A STALL
-- ---------------------------------------------------------------------------
--
-- The fill above is 107 ns a word in Factorio's Lua and there is NO fixed cost
-- to amortise it against -- measured over four increments at three heap sizes
-- (scripts/run-growprobe.sh), the least-squares intercept is NEGATIVE at every
-- size, and reaching 40 MiB in 640 grows of one wasm page costs 0.984x what
-- reaching it in 10 grows of 4 MiB costs. So the whole of a grow's cost is the
-- words, and the only question is WHICH TICK they land on. Left alone that is
-- 22.7-30.0 ms at a 3.5 MiB heap and 288-365 ms at 40 MiB, which by sharding
-- stage C is the worst tick a growing guest has -- the collector's own worst
-- paced step is 1.2x its budget in the same runs.
--
-- FILL is the word index up to which the shard vector is MATERIALISED. It is at
-- least MEMSIZE/4 and may be ahead of it, and the words between the two are
-- zero BY CONSTRUCTION rather than by convention: every path that can write a
-- word -- every emitted access, ld*/st*/st8raw, mem_copy, mem_fill, fk_wstr and
-- the host's own writes -- opens with a bounds check against MEMSIZE, so
-- nothing in the guest or the host can reach them. That is what lets a grow
-- into pre-built words do NOTHING but move the cursor: measured at 1.2-2.7 us
-- against the 1.2-116 ms the same grow costs unbuilt.
--
-- The pre-build itself is paced, and pacing it is nearly free: the same 2 MiB
-- shard built in 8,192-word pieces costs 1.02-1.03x the one-shot fill. So the
-- work is not avoided -- it cannot be, a Lua slot exists only once something
-- writes it -- it is moved onto ticks nobody is waiting on, exactly as the
-- collector's own work is.
--
-- WHAT IT IS NOT. This is not lazy materialisation. A shard whose slots are
-- created only when the guest writes them keeps its keys in the HASH part
-- rather than the array part, which is the one thing the whole shard design
-- exists to prevent; and reading an absent word would need an `__index`
-- metamethod, which `storage` cannot carry under --persist=table anyway. Both
-- are refusals, not omissions.
local mem_grow
do
  -- PREAHEAD is how far ahead of MEMSIZE the pre-build ever works: 1 MiB.
  --
  -- It is a COST, so it is bounded rather than proportional. A materialised
  -- word above MEMSIZE is a real Lua slot: 16 B of host RAM, 2.29 B of save
  -- under --persist=table, and its share of the 0.202 ms/MiB Lua's own
  -- collector spends walking the memory. One MiB of lookahead is 4 MiB of host
  -- RAM and 586 KiB of save, and it is enough to cover a full fkgc grow
  -- increment plus a megabyte-sized single allocation.
  --
  -- A PROPORTIONAL lookahead was considered and refused. TinyGo's growHeap
  -- DOUBLES, so "one grow ahead" for a leaking guest means materialising the
  -- whole next doubling -- which permanently doubles the footprint of a guest
  -- that then stops growing. The cost of being wrong is unbounded and the
  -- benefit is capped by how long the guest takes to get there.
  local PREAHEAD = 262144

  -- FILL is the materialisation cursor, in WORDS. TARGET is where the paced
  -- pre-build is working toward; TARGET <= FILL means nothing is owed.
  local FILL, TARGET = 0, 0

  -- NOTIFY is control.lua's "there is pre-build work owed" callback, installed
  -- through MEMPACK.grow_hook. It is called ONLY from the arming path of a real
  -- grow, so a guest that never calls memory.grow -- which is most guests,
  -- since TinyGo's initial memory already covers them -- pays nothing at all:
  -- no callback, no registration, no per-tick handler and not one extra word.
  local NOTIFY

  -- The fill loop, shared by the grow and the pre-build so there is ONE place
  -- that knows how a shard is created and filled. Two copies of it is how the
  -- last partial shard gets built wrong.
  --
  -- A wasm page is 16,384 words and a shard is 524,288 = 32 pages, so a shard
  -- boundary is always page-aligned and a grow never splits a word.
  local function fill(mem, from, to)
    local w = from
    while w < to do
      local off = w % 524288
      local s = (w - off) / 524288 + 1
      local t = mem[s]
      if not t then t = {} mem[s] = t end
      local k = 524288 - off
      if k > to - w then k = to - w end
      for i = off + 1, off + k do t[i] = 0 end
      w = w + k
    end
  end

  mem_grow = function(mem, size, maxbytes, pages)
    local old = size / 65536
    local want = size + pages * 65536
    if pages ~= 0 and want <= maxbytes then
      local last = want / 4
      local w = size / 4
      -- The pre-build has already created and zeroed everything below FILL, and
      -- nothing can have written it since. This is the whole win.
      if w < FILL then w = FILL end
      fill(mem, w, last)
      if last > FILL then FILL = last end

      -- ARM THE LOOKAHEAD, aiming one grow of this size ahead. The next grow is
      -- the best available predictor of the one after it: fkgc's law is a
      -- quarter of the current heap and TinyGo's is a doubling, and both are
      -- monotone, so "at least as much again" under-predicts rather than over-
      -- builds. The cap is what keeps a doubling guest from asking for the moon.
      --
      -- size ~= 0 IS THE GUARD THAT KEEPS AN IDLE GUEST FREE. The chunk builds
      -- its declared memory through this same function with size = 0, and that
      -- is a construction rather than a grow; arming there would give every
      -- guest in existence a megabyte of lookahead it never asked for.
      if size ~= 0 then
        local t = last + (want - size) / 4
        local lim = last + PREAHEAD
        if t > lim then t = lim end
        lim = maxbytes / 4
        if t > lim then t = lim end
        if t > FILL then
          TARGET = t
          if NOTIFY then NOTIFY() end
        end
      end
    else
      want = size
      if pages ~= 0 then old = 4294967295.0 end
    end
    local sb = want
    if sb > 2097152 then sb = 2097152 end
    return old, want, sb
  end

  -- One paced piece of the pre-build. Returns true while more is owed, which is
  -- what control.lua's one-shot on_tick re-registers on -- the same shape as
  -- fk_gc_step's phase return and fk.defer's flush, and for the same reason: a
  -- guest that is not growing must carry no per-tick handler.
  MEMPACK.prebuild = function(mem, budget)
    local w = FILL
    if w >= TARGET then return false end
    local to = w + budget
    if to > TARGET then to = TARGET end
    fill(mem, w, to)
    FILL = to
    return to < TARGET
  end

  MEMPACK.grow_hook = function(f) NOTIFY = f end

  -- adopt and MEMPACK.restore replace the memory wholesale, and the cursor is
  -- derived state that has to move with it or it is the same silent failure
  -- agents/sharding.md section 11 lists second: a cursor left above the new
  -- size claims words are materialised in a vector that has never seen them,
  -- and the next grow hands the guest nil-valued memory. n/4 is deliberately
  -- CONSERVATIVE -- an adopted vector may well have slots above n, and re-
  -- zeroing them on the next grow is what memory.grow requires anyway.
  MEMPACK.memreset = function(n) FILL = n / 4 TARGET = 0 end
end

-- ---------------------------------------------------------------------------
-- Floating point.
--
-- A Lua number IS an IEEE-754 double, so f64 arithmetic is native and costs
-- nothing. f32 is the work: every f32 operation must round its result to single
-- precision, or errors accumulate and diverge from the spec.
--
-- The rounding is arithmetic rather than string.pack-based, for two reasons:
-- string.pack allocates a string per operation, which puts GC pressure into a
-- lockstep game loop; and the arithmetic form measured ~5.6x faster.
-- ---------------------------------------------------------------------------

-- The constants are block-scoped, so they cost the chunk no names. rne and f32
-- stay chunk-level because both are used further down. F32_MAX is gone: it was
-- defined and never read.
local rne, f32
do
  local F32_SPLIT = 536870913.0            -- 2^29 + 1, Dekker's constant
  -- The midpoint between FLT_MAX and 2^128. Round-half-to-even sends the tie
  -- itself AWAY to infinity, so the comparison below is >=, not >.
  local F32_OVERFLOW = 3.4028235677973366e38
  local F32_MIN_NORMAL = 1.1754943508222875e-38  -- 2^-126
  local F32_DENORM_Q = 1.401298464324817e-45     -- 2^-149, the subnormal quantum

  -- Round half to even, matching IEEE-754's default mode.
  function rne(x)
    local f = x - x % 1.0
    local d = x - f
    if d > 0.5 then return f + 1.0 end
    if d < 0.5 then return f end
    if f % 2.0 == 0.0 then return f end
    return f + 1.0
  end

  function f32(v)
    if v ~= v then return v end                      -- NaN stays NaN
    if v == huge or v == -huge then return v end
    local a = v < 0.0 and -v or v
    if a >= F32_OVERFLOW then
      return v < 0.0 and -huge or huge               -- overflows to infinity
    end
    if a < F32_MIN_NORMAL then
      -- Subnormals have a fixed absolute quantum, so Dekker's relative split
      -- does not apply; round to the nearest multiple of 2^-149 instead.
      if a == 0.0 then return v end
      local scaled = v / F32_DENORM_Q
      local r = rne(scaled) * F32_DENORM_Q
      -- A negative value that rounds to zero must yield NEGATIVE zero; rne
      -- returns +0 for both signs, so the sign has to be restored explicitly.
      if r == 0.0 and v < 0.0 then return -0.0 end
      return r
    end
    -- Dekker split: forces round-to-nearest-even at 24 mantissa bits, which is
    -- exactly f32 precision.
    local t = v * F32_SPLIT
    return t - (t - v)
  end
end

-- Float helpers that have no direct Lua operator.
--
-- wasm pins behaviour that IEEE leaves loose, so these cannot be one-liners:
-- min/max must propagate NaN and distinguish -0 from +0, and nearest is
-- round-half-to-EVEN rather than the round-half-away that floor(x+0.5) gives.

-- Negative zero is the whole difficulty here: `a < 0.0` is FALSE for -0.0,
-- so the magnitude has to be taken with an explicit zero-sign test or
-- copysign(-0.0, x) keeps the wrong sign.
local function negative(x)
  return x < 0.0 or (x == 0.0 and 1.0 / x < 0.0)
end

local function fmin(a, b)
  if a ~= a then return a end
  if b ~= b then return b end
  if a == 0.0 and b == 0.0 then
    -- -0 < +0 for min, which a plain comparison cannot see.
    if negative(a) then return a end
    return b
  end
  if a < b then return a end
  return b
end

local function fmax(a, b)
  if a ~= a then return a end
  if b ~= b then return b end
  if a == 0.0 and b == 0.0 then
    if not negative(a) then return a end
    return b
  end
  if a > b then return a end
  return b
end

local function copysign(a, b)
  local m = a
  if negative(m) then m = -m end
  if negative(b) then return -m end
  return m
end

local function fceil(x)
  if x ~= x or x == huge or x == -huge then return x end
  local r = x - x % 1.0
  if r < x then r = r + 1.0 end
  -- ceil(-0.5) is -0, not +0.
  if r == 0.0 and x < 0.0 then return -0.0 end
  return r
end

local function ffloor(x)
  if x ~= x or x == huge or x == -huge then return x end
  return x - x % 1.0
end

-- Truncate toward zero.
local function ftrunc(x)
  if x ~= x or x == huge or x == -huge then return x end
  if x < 0.0 then
    local r = -((-x) - (-x) % 1.0)
    if r == 0.0 then return -0.0 end
    return r
  end
  return x - x % 1.0
end

-- Round half to EVEN. floor(x + 0.5) would round half away from zero and
-- diverge from the spec on every .5 case.
local function fnearest(x)
  if x ~= x or x == huge or x == -huge or x == 0.0 then return x end
  local f = x - x % 1.0
  local d = x - f
  local r
  if d > 0.5 then
    r = f + 1.0
  elseif d < 0.5 then
    r = f
  elseif f % 2.0 == 0.0 then
    r = f
  else
    r = f + 1.0
  end
  if r == 0.0 and x < 0.0 then return -0.0 end
  return r
end

local function fsqrt(x)
  if x ~= x then return x end
  if x < 0.0 then return 0.0 / 0.0 end   -- sqrt of a negative is NaN
  return sqrt(x)
end

local function trunc_s(x)
  if x ~= x then error(TRAPS.nan, 0) end
  local t = ftrunc(x)
  if t < -2147483648.0 or t > 2147483647.0 then error(TRAPS.range, 0) end
  return t % 4294967296.0
end

local function trunc_u(x)
  if x ~= x then error(TRAPS.nan, 0) end
  local t = ftrunc(x)
  if t <= -1.0 or t > 4294967295.0 then error(TRAPS.range, 0) end
  if t < 0.0 then t = 0.0 end            -- -0.0 and (-1, 0) truncate to 0
  return t
end

-- i32 is unsigned in [0, 2^32) per Invariant A, so the signed conversion has to
-- reinterpret the top half as negative first.
local function conv_s(v)
  if v >= 2147483648.0 then return v - 4294967296.0 end
  return v
end

-- Bit-level reinterpretation between f32 and i32, via frexp/ldexp rather than
-- string.pack: measured faster and, more importantly, allocation-free.
local function f32_to_bits(x)
  if x ~= x then return 2143289344.0 end                 -- canonical quiet NaN
  local s = 0.0
  if x < 0.0 or (x == 0.0 and 1.0 / x < 0.0) then s = 2147483648.0; x = -x end
  if x == huge then return s + 2139095040.0 end
  if x == 0.0 then return s end
  local m, e = frexp(x)
  if e < -125 then                                        -- subnormal
    return s + rne(x / 1.401298464324817e-45)
  end
  local mant = rne((m * 2.0 - 1.0) * 8388608.0)
  if mant >= 8388608.0 then mant = 0.0; e = e + 1 end     -- rounding carried out
  if e + 126 >= 255 then return s + 2139095040.0 end
  return s + (e + 126) * 8388608.0 + mant
end

local function bits_to_f32(b)
  local s = 1.0
  if b >= 2147483648.0 then s = -1.0; b = b - 2147483648.0 end
  local e = (b - b % 8388608.0) / 8388608.0
  local mant = b % 8388608.0
  if e == 0 then
    if mant == 0.0 then return s * 0.0 end
    return s * mant * 1.401298464324817e-45              -- subnormal
  end
  if e == 255 then
    if mant == 0.0 then return s * huge end
    return 0.0 / 0.0
  end
  -- PE is indexed by a BIASED f64 exponent, so an f32's biased exponent needs
  -- rebasing: 2^(e-127) is PE[(e-127)+1075] = PE[e+948]. e is in [1, 254] here,
  -- the two ends having returned above, so the index is in [949, 1202].
  return s * (1.0 + mant / 8388608.0) * PE[e + 948]
end

-- Named constants the emitter uses for non-finite literals, which have no
-- exact source form.
local HUGE = huge
local NAN = 0.0 / 0.0

-- f64 memory access. An f64 is two words; frexp/ldexp keep the conversion
-- allocation-free, where string.pack would allocate a string per access.
local function ld_f64(mem, size, a)
  -- One bounds check for the 8-byte range, and the aligned pair read here
  -- rather than through two ld32 calls. `dot` is the kernel this dominates:
  -- every element of an f64 array was three nested calls deep.
  if a < 0 or a + 8 > size then trap_oob() end
  local lo, hi
  if a % 4 == 0 then
    -- The 8-byte straddle: at offset 2097148 the two words are in two shards.
    -- Bounds were checked for all eight bytes above, so mem[s + 1] exists.
    local o = a % 2097152
    local sx = (a - o) / 2097152 + 1
    local i = o / 4 + 1
    if o <= 2097144 then
      local t = mem[sx]
      lo, hi = t[i], t[i + 1]
    else
      lo, hi = mem[sx][i], mem[sx + 1][1]
    end
  else
    lo, hi = ld32(mem, size, a), ld32(mem, size, a + 4)
  end
  local s = 1.0
  if hi >= 2147483648.0 then s = -1.0; hi = hi - 2147483648.0 end
  local e = (hi - hi % 1048576.0) / 1048576.0
  local mant = (hi % 1048576.0) * 4294967296.0 + lo
  if e == 0 then
    if mant == 0.0 then return s * 0.0 end
    return s * mant * 4.9406564584124654e-324          -- subnormal
  end
  if e == 2047 then
    if mant == 0.0 then return s * huge end
    return NAN
  end
  return s * (mant + 4503599627370496.0) * PE[e]
end

local function f64_to_bits(x)
  local lo, hi
  if x ~= x then
    return 0.0, 2146959360.0                            -- canonical quiet NaN
  else
    local s = 0.0
    if x < 0.0 or (x == 0.0 and 1.0 / x < 0.0) then s = 2147483648.0; x = -x end
    if x == huge then
      lo, hi = 0.0, s + 2146435072.0
    elseif x == 0.0 then
      lo, hi = 0.0, s
    else
      local m, e = frexp(x)
      if e < -1021 then                                 -- subnormal
        local mm = x / 4.9406564584124654e-324
        lo = mm % 4294967296.0
        hi = s + (mm - lo) / 4294967296.0
      else
        local mm = (m * 2.0 - 1.0) * 4503599627370496.0
        lo = mm % 4294967296.0
        hi = s + (mm - lo) / 4294967296.0 + (e + 1022) * 1048576.0
      end
    end
  end
  return lo, hi
end

local function st_f64(mem, size, a, x)
  local lo, hi = f64_to_bits(x)
  st64(mem, size, a, lo, hi)
end

-- ---------------------------------------------------------------------------
-- Exact NaN mode (--nan=exact).
--
-- A Lua number cannot carry a NaN's sign bit or payload. In exact mode a NaN
-- whose bits matter is represented as a BOXED table instead, so those bits
-- survive constants, memory, reinterpretation and copysign.
--
-- The cost is real: because an operand may now be a table, no float operation
-- can use a plain Lua operator, so every one of them routes through a helper
-- with a type check. That is why this mode is opt-in.
--
-- Arithmetic deliberately does NOT propagate boxes. The spec lets any operation
-- produce any NaN payload, and the conformance suite compares arithmetic NaN
-- results by class, so an arithmetic op with a boxed operand simply returns a
-- plain canonical NaN. Boxes only need to survive the paths where bits are read
-- or written.
-- ---------------------------------------------------------------------------

local function isbox(x) return type(x) == "table" end

-- A boxed NaN. nb is the low word (or the whole f32 pattern); nh is the high
-- word for an f64 and nil for an f32.
local function boxf32(bits) return { nb = bits } end
local function boxf64(lo, hi) return { nb = lo, nh = hi } end

-- Unbox to a plain number for code that only cares whether it is a NaN.
local function unbox(x)
  if isbox(x) then return NAN end
  return x
end

local function xf32(v)
  if isbox(v) then return v end
  return f32(v)
end

-- Arithmetic: a boxed operand collapses to a canonical NaN.
local function xadd(a, b) if isbox(a) or isbox(b) then return NAN end return a + b end
local function xsub(a, b) if isbox(a) or isbox(b) then return NAN end return a - b end
local function xmul(a, b) if isbox(a) or isbox(b) then return NAN end return a * b end
local function xdiv(a, b) if isbox(a) or isbox(b) then return NAN end return a / b end

local function xmin(a, b) if isbox(a) or isbox(b) then return NAN end return fmin(a, b) end
local function xmax(a, b) if isbox(a) or isbox(b) then return NAN end return fmax(a, b) end
local function xsqrt(a) if isbox(a) then return NAN end return fsqrt(a) end
local function xceil(a) if isbox(a) then return NAN end return fceil(a) end
local function xfloor(a) if isbox(a) then return NAN end return ffloor(a) end
local function xtrunc(a) if isbox(a) then return NAN end return ftrunc(a) end
local function xnearest(a) if isbox(a) then return NAN end return fnearest(a) end

-- abs and neg manipulate the sign bit, which a box actually has.
local function xabs(a)
  if isbox(a) then
    if a.nh then return boxf64(a.nb, a.nh % 2147483648.0) end
    return boxf32(a.nb % 2147483648.0)
  end
  if negative(a) then return -a end
  return a
end

local function xneg(a)
  if isbox(a) then
    local flip = function(h)
      if h >= 2147483648.0 then return h - 2147483648.0 end
      return h + 2147483648.0
    end
    if a.nh then return boxf64(a.nb, flip(a.nh)) end
    return boxf32(flip(a.nb))
  end
  return -a
end

-- copysign is the one arithmetic op where a NaN's sign reaches a result that is
-- not a NaN, which is exactly why it needs the box.
local function xcopysign(a, b)
  local bneg
  if isbox(b) then
    local top = b.nh or b.nb
    bneg = top >= 2147483648.0
  else
    bneg = negative(b)
  end
  if isbox(a) then
    local sign = bneg and 2147483648.0 or 0.0
    if a.nh then return boxf64(a.nb, a.nh % 2147483648.0 + sign) end
    return boxf32(a.nb % 2147483648.0 + sign)
  end
  local m = a
  if negative(m) then m = -m end
  if bneg then return -m end
  return m
end

-- Comparisons are false for any NaN, boxed or not.
local function xeq(a, b) if isbox(a) or isbox(b) then return 0 end return a == b and 1 or 0 end
local function xne(a, b) if isbox(a) or isbox(b) then return 1 end return a ~= b and 1 or 0 end
local function xlt(a, b) if isbox(a) or isbox(b) then return 0 end return a < b and 1 or 0 end
local function xgt(a, b) if isbox(a) or isbox(b) then return 0 end return a > b and 1 or 0 end
local function xle(a, b) if isbox(a) or isbox(b) then return 0 end return a <= b and 1 or 0 end
local function xge(a, b) if isbox(a) or isbox(b) then return 0 end return a >= b and 1 or 0 end

-- Bit access: the whole reason the box exists.
local function xf32_to_bits(x)
  if isbox(x) then return x.nb end
  return f32_to_bits(x)
end

local function xbits_to_f32(b)
  -- Only a NaN pattern needs boxing; every other value is exact as a number.
  local e = (b % 2147483648.0)
  if e >= 2139095040.0 and e % 8388608.0 ~= 0.0 then return boxf32(b) end
  return bits_to_f32(b)
end

local function xf64_to_bits(x)
  if isbox(x) then return x.nb, x.nh end
  return f64_to_bits(x)
end

local function xbits_to_f64(lo, hi)
  local e = hi % 2147483648.0
  if e >= 2146435072.0 and (e % 1048576.0 ~= 0.0 or lo ~= 0.0) then
    return boxf64(lo, hi)
  end
  -- Reconstruct through the same path a plain load uses.
  local s = 1.0
  local h = hi
  if h >= 2147483648.0 then s = -1.0; h = h - 2147483648.0 end
  local ex = (h - h % 1048576.0) / 1048576.0
  local mant = (h % 1048576.0) * 4294967296.0 + lo
  if ex == 0 then
    if mant == 0.0 then return s * 0.0 end
    return s * mant * 4.9406564584124654e-324
  end
  if ex == 2047 then
    if mant == 0.0 then return s * huge end
    return NAN
  end
  return s * (mant + 4503599627370496.0) * PE[ex]
end

local function xld_f32(mem, size, a) return xbits_to_f32(ld32(mem, size, a)) end
local function xst_f32(mem, size, a, x) st32(mem, size, a, xf32_to_bits(x)) end

local function xld_f64(mem, size, a)
  if a < 0 or a + 8 > size then trap_oob() end
  if a % 4 == 0 then
    -- Same straddle as ld_f64, and the same argument: the bounds check above
    -- covered both words, so the second shard exists.
    local o = a % 2097152
    local s = (a - o) / 2097152 + 1
    local i = o / 4 + 1
    if o <= 2097144 then
      local t = mem[s]
      return xbits_to_f64(t[i], t[i + 1])
    end
    return xbits_to_f64(mem[s][i], mem[s + 1][1])
  end
  return xbits_to_f64(ld32(mem, size, a), ld32(mem, size, a + 4))
end

local function xst_f64(mem, size, a, x)
  local lo, hi = xf64_to_bits(x)
  st64(mem, size, a, lo, hi)
end

-- Conversions out of float land: a box is a NaN, which traps.
local function xtrunc_s(x) if isbox(x) then return trunc_s(NAN) end return trunc_s(x) end
local function xtrunc_u(x) if isbox(x) then return trunc_u(NAN) end return trunc_u(x) end

-- demote/promote lose the payload by spec anyway once the width changes, so a
-- boxed operand collapses rather than trying to re-map its bits.
local function xdemote(x) if isbox(x) then return NAN end return f32(x) end
local function xpromote(x) if isbox(x) then return NAN end return x end

-- ---------------------------------------------------------------------------
-- i64 as a (lo, hi) pair of unsigned doubles.
--
-- Never a boxed table: boxing would cost an allocation and a metamethod
-- dispatch per operation, putting GC pressure into a lockstep game loop. The
-- pair travels through Lua's native multiple return, so an i64-returning
-- function needs no packing at all.
--
-- Every helper takes and returns halves in (lo, hi) order.
-- ---------------------------------------------------------------------------

local function i64_add(alo, ahi, blo, bhi)
  local lo = alo + blo
  local hi = ahi + bhi
  if lo >= 4294967296.0 then lo = lo - 4294967296.0; hi = hi + 1.0 end
  return lo, hi % 4294967296.0
end

local function i64_sub(alo, ahi, blo, bhi)
  local lo = alo - blo
  local hi = ahi - bhi
  if lo < 0.0 then lo = lo + 4294967296.0; hi = hi - 1.0 end
  return lo, hi % 4294967296.0
end

-- A full 32x32 -> 64 product. Both halves are needed, so this cannot reuse
-- mul32, which discards the top.
local function mul32_full(a, b)
  -- 1.52587890625e-05 is 2^-16 exactly; the name lives in mul32's block above.
  local ah = floor32(a * 1.52587890625e-05); local al = a - ah * 65536.0
  local bh = floor32(b * 1.52587890625e-05); local bl = b - bh * 65536.0
  local ll = al * bl
  local mid = al * bh + ah * bl
  local midlo = mid % 65536.0
  local lo = ll + midlo * 65536.0
  local carry = 0.0
  if lo >= 4294967296.0 then carry = floor32(lo * 2.3283064365386963e-10); lo = lo - carry * 4294967296.0 end
  local hi = ah * bh + (mid - midlo) / 65536.0 + carry
  return lo, hi % 4294967296.0
end

local function i64_mul(alo, ahi, blo, bhi)
  -- Only the low 64 bits survive, so ahi*bhi (starting at bit 64) is dropped.
  --
  -- The two cross terms must go through mul32 rather than being multiplied
  -- directly: alo*bhi reaches 2^64, a double keeps 53 bits, and the rounded
  -- product poisons the high word. Only the LOW 32 bits of each cross term
  -- reach bits 32..63 anyway, which is exactly what mul32 computes exactly.
  local lo, hi = mul32_full(alo, blo)
  hi = (hi + mul32(alo, bhi) + mul32(ahi, blo)) % 4294967296.0
  return lo, hi
end

local function i64_eqz(lo, hi) return (lo == 0.0 and hi == 0.0) and 1 or 0 end
local function i64_eq(alo, ahi, blo, bhi) return (alo == blo and ahi == bhi) and 1 or 0 end
local function i64_ne(alo, ahi, blo, bhi) return (alo ~= blo or ahi ~= bhi) and 1 or 0 end

local function i64_ltu(alo, ahi, blo, bhi)
  if ahi ~= bhi then return ahi < bhi and 1 or 0 end
  return alo < blo and 1 or 0
end
local function i64_gtu(alo, ahi, blo, bhi) return i64_ltu(blo, bhi, alo, ahi) end
local function i64_leu(alo, ahi, blo, bhi) return i64_gtu(alo, ahi, blo, bhi) == 1 and 0 or 1 end
local function i64_geu(alo, ahi, blo, bhi) return i64_ltu(alo, ahi, blo, bhi) == 1 and 0 or 1 end

-- Signed compares differ only in how the top half is read.
local function sgn(hi) if hi >= 2147483648.0 then return hi - 4294967296.0 end return hi end
local function i64_lts(alo, ahi, blo, bhi)
  local sa, sb = sgn(ahi), sgn(bhi)
  if sa ~= sb then return sa < sb and 1 or 0 end
  return alo < blo and 1 or 0
end
local function i64_gts(alo, ahi, blo, bhi) return i64_lts(blo, bhi, alo, ahi) end
local function i64_les(alo, ahi, blo, bhi) return i64_gts(alo, ahi, blo, bhi) == 1 and 0 or 1 end
local function i64_ges(alo, ahi, blo, bhi) return i64_lts(alo, ahi, blo, bhi) == 1 and 0 or 1 end

-- Shifts take their distance mod 64, and split on whether it crosses the halves.
local function i64_shl(alo, ahi, blo)
  local n = blo % 64.0
  if n == 0.0 then return alo, ahi end
  if n >= 32.0 then
    local d = P2[n - 32]
    return 0.0, (alo % P2[64 - n]) * d
  end
  local d = P2[n]
  local keep = P2[32 - n]
  return (alo % keep) * d,
         (ahi % keep) * d + (alo - alo % keep) / keep
end

local function i64_shru(alo, ahi, blo)
  local n = blo % 64.0
  if n == 0.0 then return alo, ahi end
  if n >= 32.0 then
    local d = P2[n - 32]
    return (ahi - ahi % d) / d, 0.0
  end
  local d = P2[n]
  return (alo - alo % d) / d + (ahi % d) * P2[32 - n],
         (ahi - ahi % d) / d
end

local function i64_shrs(alo, ahi, blo)
  local n = blo % 64.0
  if n == 0.0 then return alo, ahi end
  local neg = ahi >= 2147483648.0
  local fill = neg and 4294967295.0 or 0.0
  if n >= 32.0 then
    local d = P2[n - 32]
    local t = (ahi - ahi % d) / d
    if neg then t = t + (4294967296.0 - P2[32 - (n - 32)]) % 4294967296.0 end
    if n == 32.0 then t = ahi end
    return t, fill
  end
  local d = P2[n]
  local hi = (ahi - ahi % d) / d
  if neg then hi = hi + (4294967296.0 - P2[32 - n]) end
  return (alo - alo % d) / d + (ahi % d) * P2[32 - n], hi % 4294967296.0
end

local function i64_rotl(alo, ahi, blo)
  local n = blo % 64.0
  if n == 0.0 then return alo, ahi end
  local llo, lhi = i64_shl(alo, ahi, n)
  local rlo, rhi = i64_shru(alo, ahi, 64.0 - n)
  return llo + rlo, lhi + rhi
end

local function i64_rotr(alo, ahi, blo)
  local n = blo % 64.0
  if n == 0.0 then return alo, ahi end
  return i64_rotl(alo, ahi, 64.0 - n)
end

local function i64_clz(lo, hi)
  if hi ~= 0.0 then return clz32(hi) end
  return 32.0 + clz32(lo)
end

local function i64_ctz(lo, hi)
  if lo ~= 0.0 then return ctz32(lo) end
  return 32.0 + ctz32(hi)
end

local function i64_popcnt(lo, hi) return popcnt32(lo) + popcnt32(hi) end

-- Unsigned 64-bit division by shift-subtract.
--
-- Deliberately simple rather than fast: a compiler strength-reduces most 64-bit
-- division away, and both flagship guests have a 32-bit int, so this is far off
-- any hot path. Correctness first; a 16-bit-digit long division is the
-- optimisation if a workload ever needs it.
local function u64_divmod(alo, ahi, blo, bhi)
  if blo == 0.0 and bhi == 0.0 then trap_div0() end
  local qlo, qhi = 0.0, 0.0
  local rlo, rhi = 0.0, 0.0
  for i = 63, 0, -1 do
    -- r <<= 1
    rhi = (rhi * 2.0 + (rlo >= 2147483648.0 and 1.0 or 0.0)) % 4294967296.0
    rlo = (rlo * 2.0) % 4294967296.0
    -- bring down bit i of the dividend
    local bit
    if i >= 32 then
      local d = P2[i - 32]
      bit = ((ahi - ahi % d) / d) % 2.0
    else
      local d = P2[i]
      bit = ((alo - alo % d) / d) % 2.0
    end
    rlo = rlo + bit
    -- if r >= b then r -= b; set bit i of q
    if rhi > bhi or (rhi == bhi and rlo >= blo) then
      rlo, rhi = i64_sub(rlo, rhi, blo, bhi)
      if i >= 32 then qhi = qhi + P2[i - 32] else qlo = qlo + P2[i] end
    end
  end
  return qlo, qhi, rlo, rhi
end

local function i64_divu(alo, ahi, blo, bhi)
  local qlo, qhi = u64_divmod(alo, ahi, blo, bhi)
  return qlo, qhi
end

local function i64_remu(alo, ahi, blo, bhi)
  local _, _, rlo, rhi = u64_divmod(alo, ahi, blo, bhi)
  return rlo, rhi
end

-- Negate a pair, for the signed forms.
local function i64_neg(lo, hi) return i64_sub(0.0, 0.0, lo, hi) end

local function i64_divs(alo, ahi, blo, bhi)
  if blo == 0.0 and bhi == 0.0 then trap_div0() end
  local na = ahi >= 2147483648.0
  local nb = bhi >= 2147483648.0
  -- The one case with no representable answer: -2^63 / -1.
  if na and alo == 0.0 and ahi == 2147483648.0 and nb and blo == 4294967295.0
     and bhi == 4294967295.0 then
    error(TRAPS.range, 0)
  end
  local ulo, uhi = alo, ahi
  if na then ulo, uhi = i64_neg(ulo, uhi) end
  local vlo, vhi = blo, bhi
  if nb then vlo, vhi = i64_neg(vlo, vhi) end
  local qlo, qhi = u64_divmod(ulo, uhi, vlo, vhi)
  if na ~= nb then qlo, qhi = i64_neg(qlo, qhi) end
  return qlo, qhi
end

local function i64_rems(alo, ahi, blo, bhi)
  if blo == 0.0 and bhi == 0.0 then trap_div0() end
  local na = ahi >= 2147483648.0
  local nb = bhi >= 2147483648.0
  local ulo, uhi = alo, ahi
  if na then ulo, uhi = i64_neg(ulo, uhi) end
  local vlo, vhi = blo, bhi
  if nb then vlo, vhi = i64_neg(vlo, vhi) end
  -- rem_s does NOT trap on -2^63 % -1; the answer is 0, which falls out here.
  local _, _, rlo, rhi = u64_divmod(ulo, uhi, vlo, vhi)
  if na then rlo, rhi = i64_neg(rlo, rhi) end
  return rlo, rhi
end

-- i64 <-> float conversions.
local function i64_to_f64_u(lo, hi) return hi * 4294967296.0 + lo end

local function i64_to_f64_s(lo, hi)
  if hi >= 2147483648.0 then
    -- Negative: negate the pair, convert, negate the result.
    local nlo, nhi = i64_sub(0.0, 0.0, lo, hi)
    return -(nhi * 4294967296.0 + nlo)
  end
  return hi * 4294967296.0 + lo
end

-- i64 -> f32 must NOT go through f64 first.
--
-- Converting to a double and then rounding to single rounds TWICE, and the two
-- roundings compose to the wrong answer whenever the first one lands exactly on
-- an f32 tie: 0x20000040000001 is 2^29+1 above 2^53, so it should round UP to
-- 0x1.000001p+53, but the double rounding puts it exactly half an f32 ulp above
-- 2^53 and ties-to-even then rounds it back DOWN. The spec suite tests this
-- deliberately (conversions.wast lines 471-474 and 520-523).
--
-- The fix is round-to-odd. Drop the excess low bits, then force the surviving
-- value's lowest bit to 1 if anything nonzero was dropped. The sticky bit means
-- the intermediate is never itself a tie, so the one remaining rounding is the
-- correct one. 53 bits of intermediate is comfortably past the 2*24+2 the
-- theorem needs.
local function i64_to_f32_u(lo, hi)
  local n
  if hi ~= 0.0 then n = 64.0 - clz32(hi) else n = 32.0 - clz32(lo) end
  if n <= 53.0 then return f32(hi * 4294967296.0 + lo) end
  local k = n - 53.0                                    -- at most 11
  local d = P2[k]
  local sticky = lo % d ~= 0.0                          -- k < 32, so all dropped bits are in lo
  local rlo, rhi = i64_shru(lo, hi, k)
  if sticky then rlo = rlo - rlo % 2.0 + 1.0 end
  return f32((rhi * 4294967296.0 + rlo) * d)
end

local function i64_to_f32_s(lo, hi)
  if hi >= 2147483648.0 then
    local nlo, nhi = i64_sub(0.0, 0.0, lo, hi)
    return -i64_to_f32_u(nlo, nhi)
  end
  return i64_to_f32_u(lo, hi)
end

local function bits_to_f64(lo, hi)
  local s = 1.0
  local h = hi
  if h >= 2147483648.0 then s = -1.0; h = h - 2147483648.0 end
  local e = (h - h % 1048576.0) / 1048576.0
  local mant = (h % 1048576.0) * 4294967296.0 + lo
  if e == 0 then
    if mant == 0.0 then return s * 0.0 end
    return s * mant * 4.9406564584124654e-324
  end
  if e == 2047 then
    if mant == 0.0 then return s * huge end
    return 0.0 / 0.0
  end
  return s * (mant + 4503599627370496.0) * PE[e]
end

-- i64.trunc_* traps on NaN and on anything outside the destination range,
-- exactly as the 32-bit forms do.
local function i64_trunc_s(x)
  if x ~= x then error(TRAPS.nan, 0) end
  local t = ftrunc(x)
  if t < -9223372036854775808.0 or t >= 9223372036854775808.0 then
    error(TRAPS.range, 0)
  end
  local neg = t < 0.0
  if neg then t = -t end
  local hi = floor32(t * 2.3283064365386963e-10)
  local lo = t - hi * 4294967296.0
  if neg then return i64_sub(0.0, 0.0, lo, hi) end
  return lo, hi
end

local function i64_trunc_u(x)
  if x ~= x then error(TRAPS.nan, 0) end
  local t = ftrunc(x)
  if t <= -1.0 or t >= 18446744073709551616.0 then error(TRAPS.range, 0) end
  if t < 0.0 then t = 0.0 end
  local hi = floor32(t * 2.3283064365386963e-10)
  return t - hi * 4294967296.0, hi
end

-- Exact-mode i64 truncation, the 64-bit twin of xtrunc_s/xtrunc_u.
--
-- A box IS a NaN and so must trap, but `x ~= x` is FALSE for a table, so an
-- unwrapped box slips past the NaN check and reaches the range comparison,
-- where Lua raises "attempt to compare table with number" instead of the wasm
-- trap. Unboxing to a plain NaN first restores the right error.
local function xi64_trunc_s(x) if isbox(x) then return i64_trunc_s(NAN) end return i64_trunc_s(x) end
local function xi64_trunc_u(x) if isbox(x) then return i64_trunc_u(NAN) end return i64_trunc_u(x) end

-- Saturating float->int. Unlike the trapping forms these clamp: NaN becomes 0,
-- and anything outside the destination range becomes its nearest bound.
--
-- TinyGo's wasm-unknown target emits these unconditionally (+nontrapping-fptoint),
-- so they are not optional for our flagship guest.
local function trunc_sat_s(x)
  if x ~= x then return 0.0 end
  if x <= -2147483648.0 then return 2147483648.0 end   -- -2^31 as unsigned
  if x >= 2147483647.0 then return 2147483647.0 end
  return ftrunc(x) % 4294967296.0
end

local function trunc_sat_u(x)
  if x ~= x then return 0.0 end
  if x <= 0.0 then return 0.0 end
  if x >= 4294967295.0 then return 4294967295.0 end
  return ftrunc(x)
end

local function i64_trunc_sat_s(x)
  if x ~= x then return 0.0, 0.0 end
  if x <= -9223372036854775808.0 then return 0.0, 2147483648.0 end  -- -2^63
  if x >= 9223372036854775807.0 then return 4294967295.0, 2147483647.0 end
  local t = ftrunc(x)
  local neg = t < 0.0
  if neg then t = -t end
  local hi = floor32(t * 2.3283064365386963e-10)
  local lo = t - hi * 4294967296.0
  if neg then return i64_sub(0.0, 0.0, lo, hi) end
  return lo, hi
end

local function i64_trunc_sat_u(x)
  if x ~= x then return 0.0, 0.0 end
  if x <= 0.0 then return 0.0, 0.0 end
  if x >= 18446744073709551615.0 then return 4294967295.0, 4294967295.0 end
  local t = ftrunc(x)
  local hi = floor32(t * 2.3283064365386963e-10)
  return t - hi * 4294967296.0, hi
end

-- Exact mode: a boxed NaN is a table, and `x ~= x` is FALSE for a table, so the
-- NaN check has to unbox first or a box slips through into a comparison.
local function xtrunc_sat_s(x) if isbox(x) then return 0.0 end return trunc_sat_s(x) end
local function xtrunc_sat_u(x) if isbox(x) then return 0.0 end return trunc_sat_u(x) end
local function xi64_trunc_sat_s(x)
  if isbox(x) then return 0.0, 0.0 end
  return i64_trunc_sat_s(x)
end
local function xi64_trunc_sat_u(x)
  if isbox(x) then return 0.0, 0.0 end
  return i64_trunc_sat_u(x)
end
