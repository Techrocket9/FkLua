-- FkLua day-0 sandbox probe.
--
-- Measures the exact shape of Factorio's Lua 5.2 sandbox so the compiler is
-- calibrated against observed behaviour rather than inference. Writes
-- script-output/fklua/probe.json, and logs timing lines tagged FKPROBE_TIME
-- for the harness to scrape out of factorio-current.log.
--
-- Timing has to go through the log because helpers.create_profiler() is the
-- only clock in the sandbox (os is absent) and it deliberately refuses to hand
-- Lua a raw number -- a LuaProfiler can only be rendered into a LocalisedString.

local R = { meta = {}, checks = {}, values = {}, errors = {} }

-- Records a boolean observation.
local function obs(key, value)
  R.checks[key] = value and true or false
end

-- Records an arbitrary observed value, stringifying anything non-scalar so
-- table_to_json never chokes.
local function val(key, v)
  local t = type(v)
  if t == "number" or t == "string" or t == "boolean" then
    R.values[key] = v
  else
    R.values[key] = tostring(v)
  end
end

-- Runs fn, recording either its returned value or the error text. The probe
-- must never take the game down, and "what is the error message" is frequently
-- the actual finding.
local function try(key, fn)
  local ok, res = pcall(fn)
  if ok then
    val(key, res)
  else
    R.errors[key] = tostring(res)
    R.values[key] = "<error>"
  end
  return ok, res
end

local function timed(label, reps, fn)
  local p = helpers.create_profiler()
  fn()
  p.stop()
  log({ "", "FKPROBE_TIME\t" .. label .. "\t" .. reps .. "\t", p })
end

-- ===========================================================================
-- 1. string.pack / unpack / packsize on a doubles-only build   [CRITICAL]
--
-- Factorio backports these from Lua 5.4.6, where LUA_INTEGER is a real 64-bit
-- integer type. Stock 5.2 has no integer subtype and lua_tointegerx does a
-- truncating C cast, so a naive backport would silently accept 3.5 where 5.4
-- raises "number has no integer representation". Which behaviour we actually
-- get decides the whole string-marshalling design, and whether <I8> is usable
-- at all for the i64 path.
-- ===========================================================================
local function probe_string_pack()
  obs("string_pack_present", type(string.pack) == "function")
  obs("string_unpack_present", type(string.unpack) == "function")
  obs("string_packsize_present", type(string.packsize) == "function")
  if type(string.pack) ~= "function" then return end

  try("pack_I4_integral", function()
    return #string.pack("<I4", 4000000000)
  end)
  try("pack_I4_roundtrip", function()
    return (string.unpack("<I4", string.pack("<I4", 4000000000)))
  end)

  -- The decisive question: truncate (5.2-style cast) or raise (5.4-style)?
  try("pack_I4_fractional", function()
    return (string.unpack("<I4", string.pack("<I4", 3.5)))
  end)

  -- 64-bit formats with no 64-bit integer type behind them.
  try("packsize_I8", function() return string.packsize("<I8") end)
  try("pack_I8_small", function()
    return (string.unpack("<I8", string.pack("<I8", 42)))
  end)
  -- 2^53+1 is not representable as a double; whatever comes back tells us
  -- whether <I8> can carry an i64 lo/hi pair losslessly.
  try("pack_I8_above_2p53", function()
    return (string.unpack("<I8", string.pack("<I8", 9007199254740993)))
  end)
  try("pack_i8_negative", function()
    return (string.unpack("<i8", string.pack("<i8", -1)))
  end)

  -- Float punning: the alternative to frexp/ldexp.
  try("pack_f32_roundtrip", function()
    return (string.unpack("<f", string.pack("<f", 3.14159265358979)))
  end)
  try("pack_f64_roundtrip", function()
    return (string.unpack("<d", string.pack("<d", 3.14159265358979)))
  end)
  -- Reinterpret a double's bits as two u32s -- the fast path we would use for
  -- f64 loads/stores if it beats frexp.
  try("pack_f64_as_2xI4", function()
    local lo, hi = string.unpack("<I4I4", string.pack("<d", 1.0))
    return string.format("lo=%d hi=%d", lo, hi)
  end)
  try("pack_endianness_marker", function()
    return (string.unpack("<I4", string.pack("<I4", 1)))
  end)
end

-- ===========================================================================
-- 2. Float helpers  --  gates the fast bit-punning path
-- ===========================================================================
local function probe_float_helpers()
  obs("math_frexp_present", type(math.frexp) == "function")
  obs("math_ldexp_present", type(math.ldexp) == "function")
  if math.frexp then
    try("frexp_of_1", function()
      local m, e = math.frexp(1.0); return string.format("m=%.17g e=%d", m, e)
    end)
  end
  if math.ldexp then
    try("ldexp_half_1", function() return math.ldexp(0.5, 1) end)
  end
  obs("math_fmod_present", type(math.fmod) == "function")
  obs("math_modf_present", type(math.modf) == "function")
  obs("math_pow_present", type(math.pow) == "function")
end

-- ===========================================================================
-- 3. bit32 completeness and signedness
-- ===========================================================================
local function probe_bit32()
  obs("bit32_present", type(bit32) == "table")
  if type(bit32) ~= "table" then return end
  for _, n in ipairs({ "band", "bor", "bxor", "bnot", "lshift", "rshift",
                       "arshift", "lrotate", "rrotate", "extract", "replace",
                       "btest" }) do
    obs("bit32_" .. n, type(bit32[n]) == "function")
  end
  try("bit32_bnot_0", function() return bit32.bnot(0) end)          -- expect 4294967295
  try("bit32_arshift_neg", function() return bit32.arshift(0x80000000, 31) end)
  try("bit32_lshift_overflow", function() return bit32.lshift(1, 31) end)
  -- Does it accept a non-integral double, and if so how does it reduce it?
  try("bit32_band_fractional", function() return bit32.band(3.7, 0xFFFFFFFF) end)
end

-- ===========================================================================
-- 4. Invariant B -- goto and local scoping
-- ===========================================================================
local function probe_goto()
  obs("goto_all_locals_first",
    load("local a,b,c a=1 ::top:: b=2 if a<1 then goto top end c=3 return c") ~= nil)
  local f, err = load("goto skip local x = 1 ::skip:: return 0")
  obs("goto_into_local_scope_rejected", f == nil)
  if err then val("goto_scope_error_text", tostring(err)) end
  obs("goto_backward_loop",
    load("local i i=0 ::top:: i=i+1 if i<10 then goto top end return i") ~= nil)
end

-- ===========================================================================
-- 5. The 200-local ceiling
-- ===========================================================================
local function probe_locals()
  local function nlocals(n)
    local t = {}
    for i = 1, n do t[i] = "local v" .. i end
    return load(table.concat(t, " ") .. " return 0")
  end
  obs("locals_199", nlocals(199) ~= nil)
  obs("locals_200", nlocals(200) ~= nil)
  obs("locals_201", nlocals(201) ~= nil)
  local _, err = nlocals(201)
  if err then val("locals_limit_error_text", tostring(err)) end

  -- Upvalue ceiling, for the M5 upvalue-promotion pass. The enclosing scope is
  -- itself capped at 200 locals, so N upvalues cannot simply be N enclosing
  -- locals past 200 -- that measures the local limit again. Nest one level so
  -- the inner function closes over a middle function's locals in batches.
  local function nupvals(n)
    local decl, use = {}, {}
    for i = 1, n do decl[i] = "local u" .. i .. "=" .. i; use[i] = "u" .. i end
    -- Chunks of <=150 locals per enclosing function level.
    local per, levels = 150, {}
    for base = 1, n, per do
      local hi = math.min(base + per - 1, n)
      local d = {}
      for i = base, hi do d[#d + 1] = decl[i] end
      levels[#levels + 1] = table.concat(d, " ")
    end
    local src = {}
    for _, lv in ipairs(levels) do src[#src + 1] = "do " .. lv .. " return function() return " end
    src[#src + 1] = table.concat(use, "+")
    for _ = 1, #levels do src[#src + 1] = " end end" end
    -- Simpler equivalent: one enclosing function per batch, innermost sums all.
    local flat = {}
    for _, lv in ipairs(levels) do flat[#flat + 1] = lv .. " " end
    return load(table.concat(flat) ..
      " return function() return " .. table.concat(use, "+") .. " end")
  end
  -- Note: still bounded by the 200-local ceiling of the enclosing chunk, so
  -- values above ~195 measure that, not the upvalue limit. Recorded for the
  -- record; the emitter budgets ~120 upvalues, far below either bound.
  obs("upvals_60", nupvals(60) ~= nil)
  obs("upvals_120", nupvals(120) ~= nil)
  obs("upvals_195", nupvals(195) ~= nil)
end

-- ===========================================================================
-- 6. load(): source accepted, bytecode rejected, and parse throughput
-- ===========================================================================
local function probe_load()
  local f = load("return 21*2")
  obs("load_source_ok", type(f) == "function" and f() == 42)
  obs("string_dump_present", type(string.dump) == "function")
  if string.dump then
    local ok, bin = pcall(string.dump, function() return 1 end)
    if ok then
      local g, err = load(bin)
      obs("load_binary_rejected", g == nil)
      if err then val("load_binary_error_text", tostring(err)) end
      local h = load(bin, "c", "b")
      obs("load_mode_b_ignored", h == nil)
    else
      R.errors.string_dump = tostring(bin)
    end
  end

  -- Parse throughput sizes the per-file function split. Functions go into a
  -- table, exactly as the emitter will produce them: `local function fN` would
  -- declare a chunk-level local per function and blow the 200-local ceiling at
  -- function 200, which is precisely why the emitter cannot do that either.
  for _, kb in ipairs({ 256, 1024, 4096 }) do
    local parts, n = { "local F = {}\n" }, 0
    local target = kb * 1024
    local i = 0
    while n < target do
      i = i + 1
      local s = "F[" .. i .. "]=function(a,b) local c=a+b if c>=4294967296.0 then c=c-4294967296.0 end return c end\n"
      parts[#parts + 1] = s
      n = n + #s
    end
    parts[#parts + 1] = "return F\n"
    local src = table.concat(parts)
    val("load_src_bytes_" .. kb .. "k", #src)
    val("load_src_funcs_" .. kb .. "k", i)
    local chunk, perr = load(src, "=probe")
    obs("load_parses_" .. kb .. "k", chunk ~= nil)
    if not chunk then
      val("load_parse_error_" .. kb .. "k", tostring(perr))
    else
      timed("load_parse_" .. kb .. "k", #src, function()
        local c = load(src, "=probe")
        if not c then error("reparse failed at " .. kb .. "k") end
      end)
    end
  end
end

-- ===========================================================================
-- 7. Functions the emitter and runtime depend on
-- ===========================================================================
local function probe_stdlib()
  for _, n in ipairs({ "pcall", "xpcall", "select", "rawget", "rawset", "rawlen",
                       "rawequal", "error", "assert", "tonumber", "tostring",
                       "type", "next", "pairs", "ipairs", "setmetatable",
                       "getmetatable", "load", "require", "print" }) do
    obs("g_" .. n, type(_G[n]) == "function")
  end
  for _, n in ipairs({ "concat", "insert", "remove", "sort", "unpack", "pack" }) do
    obs("table_" .. n, type(table[n]) == "function")
  end
  for _, n in ipairs({ "byte", "char", "sub", "rep", "format", "find", "gsub" }) do
    obs("string_" .. n, type(string[n]) == "function")
  end
  obs("debug_getinfo", type(debug) == "table" and type(debug.getinfo) == "function")
  obs("debug_traceback", type(debug) == "table" and type(debug.traceback) == "function")
  obs("debug_sethook_absent", type(debug) ~= "table" or debug.sethook == nil)
  for _, n in ipairs({ "coroutine", "io", "os", "loadfile", "dofile" }) do
    obs("absent_" .. n, rawget(_G, n) == nil)
  end
  obs("math_type_absent", math.type == nil)
  obs("no_intdiv_operator", load("return 7//2") == nil)
  obs("no_bitwise_operators", load("return 1<<2") == nil)

  -- collectgarbage: PRESENT, with every 5.2 option, and the day-0 probe never
  -- asked. It matters because a guest's linear memory is one enormous Lua array
  -- table, and at a large heap the collector is the whole per-tick story for a
  -- mod that runs no Lua at all. The answer turned out to be that it is present
  -- and does not help: Lua traverses a table in ONE indivisible propagatemark,
  -- so there is nothing for setpause/setstepmul to pace (measured -- see
  -- agents/guests.md, "the guest heap budget"). Recorded so nobody asks twice.
  -- The defaults go back on afterwards: this is one shared Lua state and the
  -- probe does not get to retune the collector for every other mod in it.
  obs("g_collectgarbage", type(collectgarbage) == "function")
  if type(collectgarbage) == "function" then
    for _, opt in ipairs({ "count", "step", "collect", "isrunning",
                           "setpause", "setstepmul" }) do
      local ok, res = pcall(collectgarbage, opt, 200)
      obs("cg_" .. opt, ok)
      if not ok then R.errors["cg_" .. opt] = tostring(res) end
    end
    collectgarbage("setpause", 200)
    collectgarbage("setstepmul", 200)
  end
end

-- ===========================================================================
-- 8. Hex float literals  --  gates exact constant emission
-- ===========================================================================
local function probe_hexfloat()
  local f = load("return 0x1.91eb851eb851fp+1")
  obs("hexfloat_parses", f ~= nil)
  if f then val("hexfloat_value", string.format("%.17g", f())) end
  local g = load("return 0x1p-1")
  obs("hexfloat_p_notation", g ~= nil and g() == 0.5)
  val("tostring_pi", tostring(3.14159265358979))
  val("format_17g_pi", string.format("%.17g", 3.14159265358979))
  obs("tonumber_hexfloat", tonumber("0x1p-1") == 0.5)
end

-- ===========================================================================
-- 9. NaN / inf behaviour
-- ===========================================================================
local function probe_float_edge()
  local nan = 0 / 0
  local inf = math.huge
  obs("nan_ne_self", nan ~= nan)
  val("tostring_nan", tostring(nan))
  val("tostring_inf", tostring(inf))
  val("tostring_neginf", tostring(-inf))
  try("nan_as_table_key", function()
    local t = {}; t[nan] = 1; return "accepted"
  end)
  val("int_max_exact", string.format("%.17g", 2 ^ 53))
  obs("2p53_plus_1_collapses", (2 ^ 53 + 1) == 2 ^ 53)
  try("neg_zero_distinguishable", function()
    return (1 / (-0.0)) == -math.huge
  end)
end

-- ===========================================================================
-- 10. Codegen micro-decisions -- the measurements that fork the emitter
-- ===========================================================================
local function probe_kernels()
  local N = 2000000
  local floor = math.floor
  local band, rshift = bit32.band, bit32.rshift

  timed("baseline_loop", N, function()
    local a = 0
    for _ = 1, N do a = a + 1 end
    return a
  end)

  -- i32.add wrap: conditional fixup vs % vs bit32.
  --
  -- Measured in BOTH directions, because real code overflows rarely: the
  -- taken-branch case is the worst case for the conditional form and the
  -- not-taken case is its best, and the honest comparison needs both.
  timed("add_wrap_cond_taken", N, function()          -- always overflows
    local a, b, s = 4000000000.0, 300000000.0, 0
    for _ = 1, N do s = a + b; if s >= 4294967296.0 then s = s - 4294967296.0 end end
    return s
  end)
  timed("add_wrap_cond_nottaken", N, function()       -- never overflows
    local a, b, s = 1000.0, 2000.0, 0
    for _ = 1, N do s = a + b; if s >= 4294967296.0 then s = s - 4294967296.0 end end
    return s
  end)
  timed("add_wrap_mod_taken", N, function()
    local a, b, s = 4000000000.0, 300000000.0, 0
    for _ = 1, N do s = (a + b) % 4294967296.0 end
    return s
  end)
  timed("add_wrap_mod_nottaken", N, function()
    local a, b, s = 1000.0, 2000.0, 0
    for _ = 1, N do s = (a + b) % 4294967296.0 end
    return s
  end)
  timed("add_nowrap", N, function()                   -- lower bound: no wrap at all
    local a, b, s = 1000.0, 2000.0, 0
    for _ = 1, N do s = a + b end
    return s
  end)
  timed("add_wrap_bit32", N, function()
    local a, b, s = 4000000000.0, 300000000.0, 0
    for _ = 1, N do s = band(a + b, 4294967295.0) end
    return s
  end)

  -- shr_u: math.floor vs fmod-style
  timed("shr_floor", N, function()
    local a, r = 3735928559.0, 0
    for _ = 1, N do r = floor(a / 256.0) end
    return r
  end)
  timed("shr_modsub", N, function()
    local a, r = 3735928559.0, 0
    for _ = 1, N do r = (a - a % 256.0) / 256.0 end
    return r
  end)
  timed("shr_bit32", N, function()
    local a, r = 3735928559.0, 0
    for _ = 1, N do r = rshift(a, 8) end
    return r
  end)

  -- i32.mul: the three candidate splits. If the magic-number floor works it is
  -- the single largest win available in the whole emitter.
  timed("mul_const_small", N, function()
    local a, r = 3735928559.0, 0
    for _ = 1, N do r = (a * 12.0) % 4294967296.0 end
    return r
  end)
  timed("mul_split_floor", N, function()
    local a, b, r = 3735928559.0, 2654435761.0, 0
    for _ = 1, N do
      local ah = floor(a * 1.52587890625e-05); local al = a - ah * 65536.0
      local bh = floor(b * 1.52587890625e-05); local bl = b - bh * 65536.0
      local m = al * bh + ah * bl
      m = m - floor(m * 1.52587890625e-05) * 65536.0
      r = al * bl + m * 65536.0
      if r >= 4294967296.0 then r = r % 4294967296.0 end
    end
    return r
  end)
  timed("mul_split_bit32", N, function()
    local a, b, r = 3735928559.0, 2654435761.0, 0
    for _ = 1, N do
      local ah = rshift(a, 16); local al = band(a, 65535)
      local bh = rshift(b, 16); local bl = band(b, 65535)
      local m = band(al * bh + ah * bl, 65535)
      r = (al * bl + m * 65536.0) % 4294967296.0
    end
    return r
  end)
  -- Magic-number floor: no C call at all, just FP rounding.
  timed("mul_split_magic", N, function()
    local a, b, r = 3735928559.0, 2654435761.0, 0
    local M = 6755399441055744.0                       -- 2^52 + 2^51
    for _ = 1, N do
      local q = a * 1.52587890625e-05
      local ah = q + M - M; if ah > q then ah = ah - 1.0 end
      local al = a - ah * 65536.0
      local q2 = b * 1.52587890625e-05
      local bh = q2 + M - M; if bh > q2 then bh = bh - 1.0 end
      local bl = b - bh * 65536.0
      local m = al * bh + ah * bl
      local q3 = m * 1.52587890625e-05
      local mh = q3 + M - M; if mh > q3 then mh = mh - 1.0 end
      r = al * bl + (m - mh * 65536.0) * 65536.0
      if r >= 4294967296.0 then r = r % 4294967296.0 end
    end
    return r
  end)
  -- Correctness of the magic-number floor, independent of its speed.
  try("magic_floor_correct", function()
    local M = 6755399441055744.0
    local bad = 0
    for _, x in ipairs({ 0.0, 1.0, 0.5, 65535.9, 65536.0, 4294967295.0,
                         3735928559.0, 2654435761.0, 2147483648.0 }) do
      local q = x * 1.52587890625e-05
      local t = q + M - M; if t > q then t = t - 1.0 end
      if t ~= math.floor(q) then bad = bad + 1 end
    end
    return bad == 0 and "exact" or ("mismatches=" .. bad)
  end)

  -- MEM indexing: is the table in Lua's array part or its hash part?
  --
  -- The index arithmetic is identical in all three so this isolates table shape
  -- rather than the cost of a +1. Keys 1..W are the array part; keys 0..W-1 put
  -- exactly one key (0) in the hash; keys OFF+1..OFF+W are entirely hash.
  local W = 65536
  local OFF = 1000000
  local mem1, mem0, memh = {}, {}, {}
  for i = 1, W do mem1[i] = i end
  for i = 0, W - 1 do mem0[i] = i end
  for i = 1, W do memh[OFF + i] = i end
  local REPS = math.floor(N / W)
  timed("mem_array_1based", REPS * W, function()
    local s, m = 0, mem1
    for _ = 1, REPS do for i = 1, W do s = s + m[i] end end
    return s
  end)
  timed("mem_array_0based", REPS * W, function()
    local s, m = 0, mem0
    for _ = 1, REPS do for i = 0, W - 1 do s = s + m[i] end end
    return s
  end)
  timed("mem_hash_part", REPS * W, function()
    local s, m = 0, memh
    for _ = 1, REPS do for i = OFF + 1, OFF + W do s = s + m[i] end end
    return s
  end)
  -- Sanity: prove the hash table really has an empty array part.
  val("mem_hash_len", #memh)
  val("mem_array_len", #mem1)

  -- Float bit-punning: frexp/ldexp vs string.pack.
  if math.frexp then
    timed("pun_frexp", N, function()
      local frexp, x, acc = math.frexp, 3.14159265358979, 0
      for _ = 1, N do local m, e = frexp(x); acc = acc + e end
      return acc
    end)
  end
  if string.pack then
    timed("pun_stringpack", N, function()
      local pack, unpack, x, acc = string.pack, string.unpack, 3.14159265358979, 0
      for _ = 1, N do local lo, hi = unpack("<I4I4", pack("<d", x)); acc = acc + hi end
      return acc
    end)
  end

  -- Call dispatch: F[idx] vs upvalue. Drives the M5 upvalue-promotion pass.
  local F = {}
  F[7] = function(x) return x + 1 end
  local up = F[7]
  timed("call_table", N, function()
    local r = 0
    for _ = 1, N do r = F[7](r) end
    return r
  end)
  timed("call_upvalue", N, function()
    local r = 0
    for _ = 1, N do r = up(r) end
    return r
  end)
end

-- ===========================================================================
-- 11. storage scale -- sets the --persist=auto threshold
-- ===========================================================================
local function probe_storage_scale()
  local W = 262144                                   -- 1 MiB guest heap in u32 words
  timed("storage_build_table", W, function()
    local t = {}
    for i = 1, W do t[i] = 0 end
    storage.probe_mem = t
  end)
  val("storage_table_entries", W)

  if string.pack then
    timed("storage_pack_pages", W, function()
      local t, pages = storage.probe_mem, {}
      -- 64 KiB per page = 16384 words, packed 256 at a time to stay well clear
      -- of the C stack limit on varargs.
      for p = 0, (W / 16384) - 1 do
        local chunks = {}
        for c = 0, (16384 / 256) - 1 do
          local base = p * 16384 + c * 256
          local acc = {}
          for k = 1, 256 do acc[k] = t[base + k] end
          chunks[#chunks + 1] = string.pack(string.rep("<I4", 256), table.unpack(acc))
        end
        pages[#pages + 1] = table.concat(chunks)
      end
      storage.probe_packed = pages
      val("storage_packed_pages", #pages)
      val("storage_packed_bytes", #pages * #pages[1])
    end)
  end
end

-- ===========================================================================
-- 12. register_metatable and pairs ordering
-- ===========================================================================
local function probe_metatable_and_order()
  obs("script_register_metatable", type(script.register_metatable) == "function")
  local t, order = {}, {}
  t.zebra = 1; t.apple = 2; t.mango = 3; t[1] = 4; t[2] = 5
  for k in pairs(t) do order[#order + 1] = tostring(k) end
  val("pairs_order", table.concat(order, ","))
end

-- ===========================================================================
-- 13. Environment
-- ===========================================================================
local function probe_env()
  R.meta.lua_version = _VERSION
  R.meta.game_version = helpers.game_version
  R.meta.tick = game and game.tick or -1
  R.meta.api_probe_version = "0.0.1"
  local mods = {}
  for name, ver in pairs(script.active_mods) do mods[name] = ver end
  R.meta.active_mods = mods
end

-- ===========================================================================

local function run()
  local sections = {
    { "env", probe_env },
    { "string_pack", probe_string_pack },
    { "float_helpers", probe_float_helpers },
    { "bit32", probe_bit32 },
    { "goto", probe_goto },
    { "locals", probe_locals },
    { "load", probe_load },
    { "stdlib", probe_stdlib },
    { "hexfloat", probe_hexfloat },
    { "float_edge", probe_float_edge },
    { "kernels", probe_kernels },
    { "storage_scale", probe_storage_scale },
    { "metatable", probe_metatable_and_order },
  }
  for _, s in ipairs(sections) do
    local ok, err = pcall(s[2])
    if not ok then
      R.errors["section_" .. s[1]] = tostring(err)
      log("FKPROBE_SECTION_FAILED\t" .. s[1] .. "\t" .. tostring(err))
    end
  end

  local nchecks, nfalse, nerr = 0, 0, 0
  for _, ok in pairs(R.checks) do
    nchecks = nchecks + 1
    if not ok then nfalse = nfalse + 1 end
  end
  for _ in pairs(R.errors) do nerr = nerr + 1 end

  helpers.write_file("fklua/probe.json", helpers.table_to_json(R), false)
  log(string.format("FKPROBE_DONE\tchecks=%d\tfalse=%d\terrors=%d",
                    nchecks, nfalse, nerr))
  game.print("[fklua-probe] wrote script-output/fklua/probe.json")
end

local done = false
script.on_event(defines.events.on_tick, function()
  if done then return end
  done = true
  local ok, err = pcall(run)
  if not ok then
    log("FKPROBE_FATAL\t" .. tostring(err))
  end
end)
