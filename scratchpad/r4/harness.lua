-- Round 4's measurement harness: the shared setup every leg runs inside.
--
-- THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE. It loads the REAL
-- runtime/lua/fk_abi.lua verbatim and the REAL emitted memio out of a compiled
-- one-page module, so every number it produces is taken through the same
-- codecs, the same handle table and the same dispatcher a packaged mod uses.
-- What it adds is a PROTOTYPE of the two shapes round 4 is designing -- a bulk
-- attribute read and a batched GUI add -- written in the style fk_abi.lua would
-- have written them, beside the real ones rather than inside them.
--
-- The prototypes live here rather than in runtime/lua for two reasons: a design
-- round may not edit shipping runtime Lua, and a prototype measured beside the
-- real path is a fairer comparison than one measured after replacing it.
--
-- Driven by bench.py, which supplies MEMLUA, ABIDIR, LEG and REPS.

package.path = ABIDIR .. "/?.lua"
local H = require("fk_abi")

-- The real emitted memio, out of a compiled `(module (memory 16))`. The chunk
-- is INLINED by the driver above this line, exactly as internal/factorio's
-- abicost harness inlines it: Factorio's sandbox has no `dofile` or `loadfile`,
-- and bin/lua52f is patched to match, which is the oracle being right.
local Mod = MODTAB
local IO = Mod.memio
H.bind_memory(IO)
H.bind_read_string(Mod.read_string)
H.bind_globals({})

-- A bump allocator over the top half of the memory, wrapping, exactly as
-- internal/factorio's abicost harness does. Nothing here allocates in the timed
-- legs; it is bound so that a string return is performed rather than refused.
local allocNext = 262144
H.bind_alloc(function(n)
  local p = allocNext
  allocNext = allocNext + n
  if allocNext > 500000 then allocNext = 262144 end
  return p
end, function() end)
H.bind_scratch(400000, 100000)

local ld8, ld32, ldf64 = IO.ld8, IO.ld32, IO.ldf64
local st8, st32, stf64 = IO.st8, IO.st32, IO.stf64
local wstr = IO.wstr

-- ---------------------------------------------------------------------------
-- The stand-in engine objects
--
-- ENGINE-SHAPED, which in this repo is a rule rather than a nicety: a Factorio
-- LuaObject hands a method back from __index as a closure already bound to the
-- object, and a plain `function(self, x)` in a table is the shape that hid an
-- arity defect on every method in the API for a milestone.
-- ---------------------------------------------------------------------------

-- The thing a polling mod reads. One numeric attribute and one string one, so
-- the two attribute shapes can be told apart.
local function newEntity(i)
  return {
    valid = true,
    health = 100.0 + i,
    name = "assembling-machine-3",
    unit_number = 1000 + i,
  }
end

-- A GUI element. `add` returns a child, which is what makes the batched form's
-- handle-return half real rather than assumed.
local child = { valid = true, name = "child", index = 1 }
local addCount = 0
local parent = {
  valid = true,
  index = 1,
}
parent.add = function(spec)
  -- Touch the spec the way the engine would: read the discriminating key and
  -- one more. Without this the decode could be optimised away by a reader who
  -- assumed the table was never used, and the leg would measure less than it
  -- claims.
  addCount = addCount + 1
  if spec.type == nil then error("add: no type") end
  local _ = spec.name
  return child
end

-- ---------------------------------------------------------------------------
-- Member signatures, written out the way factorio.Layout renders them
-- ---------------------------------------------------------------------------

local NONE = {}
-- one f64 return, no presence byte
local RET_F64 = { { kind = H.K_F64, at = 0 } }
-- one string return (ptr, len)
local RET_STR = { { kind = H.K_STR, at = 0 } }
-- one handle return
local RET_H = { { kind = H.K_HANDLE, at = 0 } }
-- one u32 return
local RET_U32 = { { kind = H.K_U32, at = 0 } }
-- one dyn argument
local ARG_DYN = { { kind = H.K_DYN, at = 0 } }
-- three u32 arguments: (ptr, count, dst)
local ARG_3U32 = {
  { kind = H.K_U32, at = 0 },
  { kind = H.K_U32, at = 4 },
  { kind = H.K_U32, at = 8 },
}

-- The prototype kinds. Numbers past the shipped set so nothing collides.
local K_BULKGET = 101
local K_BATCHADD = 102

H.bind_members({
  [1] = { kind = H.GET, name = "health", sig = { args = NONE, rets = RET_F64 } },
  [2] = { kind = H.GET, name = "name", sig = { args = NONE, rets = RET_STR } },
  [3] = { kind = H.CALL, name = "add", sig = { args = ARG_DYN, rets = RET_H } },
  [4] = { kind = K_BULKGET, name = "health", sig = { args = ARG_3U32, rets = RET_U32 } },
  [5] = { kind = K_BATCHADD, name = "add", sig = { args = ARG_3U32, rets = RET_U32 } },
  [6] = { kind = H.CALL, name = "reload_script", sig = { args = NONE, rets = NONE } },
})

-- ---------------------------------------------------------------------------
-- The world: N entities, their handles laid out in guest memory exactly as an
-- `Into` variant would have left them.
--
-- THAT IS THE DESIGN POINT THIS LAYOUT IS MAKING, not an implementation
-- convenience: an array return already writes (base, count) of a 4-byte-stride
-- handle array into guest memory, and a Go []Object is {h uint32} with the same
-- stride. So the handle array a bulk read consumes is bit-for-bit the array the
-- search that produced it already wrote, and neither side marshals anything.
-- ---------------------------------------------------------------------------

local N = COUNT
local HPTR = 4096            -- the handle array
local DSTPTR = 32768         -- the destination
local ARGP, RETP = 2048, 3072

local ents = {}
for i = 0, N - 1 do
  ents[i] = newEntity(i)
  st32(HPTR + i * 4, H.transient(ents[i]))
end
local parentH = H.transient(parent)

-- ---------------------------------------------------------------------------
-- PROTOTYPE 1: the bulk attribute read
--
-- Four arms, because the design's open question is what the per-element error
-- semantics cost. Each is the same loop with one guard removed, so the
-- difference between two rows is that guard and nothing else.
-- ---------------------------------------------------------------------------

local OK = H.OK

-- Everything a per-call GET does, per element: resolve, valid-check,
-- pcall the member read, store.
local function rawget_member(obj, name) return obj[name] end

local function bulk_full(m, argp, retp)
  local hptr, n, dst = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local name = m.name
  local nok = 0
  for i = 0, n - 1 do
    local obj, st = H.get(ld32(hptr + i * 4))
    if obj ~= nil then
      st = H.check_valid(obj, m.valid)
      if st == OK then
        local ok, v = pcall(rawget_member, obj, name)
        if ok and v ~= nil then
          stf64(dst + i * 8, v)
          nok = nok + 1
        end
      end
    end
  end
  st32(retp, nok)
  return OK
end

-- THE GENERIC ARM, and it is the one the design proposes for the general case:
-- the destination is an ARRAY OF THE GETTER'S OWN RETURN BLOCK, so the element
-- layout is `m.sig.rets` unchanged, optionality is the presence byte Layout
-- already places, and the guest decodes element i with the identical code its
-- single getter already carries. Nothing new is laid out anywhere.
local function bulk_encode_rets(m, argp, retp)
  local hptr, n, dst = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local name, stride = m.name, m.retsize
  local nok = 0
  for i = 0, n - 1 do
    local obj, st = H.get(ld32(hptr + i * 4))
    if obj ~= nil then
      st = H.check_valid(obj, m.valid)
      if st == OK then
        local ok, v = pcall(rawget_member, obj, name)
        if ok then
          H.encode_rets(m.sig, dst + i * stride, v)
          nok = nok + 1
        end
      end
    end
  end
  st32(retp, nok)
  return OK
end

-- ONE pcall for the whole batch. The arm the design has to choose between:
-- cheaper per element, and a raise at element i abandons elements i+1..n.
local function bulk_batch_pcall_body(hptr, n, dst, name)
  local nok = 0
  for i = 0, n - 1 do
    local obj = H.get(ld32(hptr + i * 4))
    if obj ~= nil and obj.valid ~= false then
      local v = obj[name]
      if v ~= nil then
        stf64(dst + i * 8, v)
        nok = nok + 1
      end
    end
  end
  return nok
end

local function bulk_batched_pcall(m, argp, retp)
  local hptr, n, dst = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local ok, nok = pcall(bulk_batch_pcall_body, hptr, n, dst, m.name)
  if not ok then return H.ERR_CALL_FAILED end
  st32(retp, nok)
  return OK
end

-- No pcall and no valid check: the floor of what any bulk form could cost,
-- which is what says how much of the per-element cost is the guards.
local function bulk_bare(m, argp, retp)
  local hptr, n, dst = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local name = m.name
  for i = 0, n - 1 do
    local obj = H.get(ld32(hptr + i * 4))
    stf64(dst + i * 8, obj[name])
  end
  st32(retp, n)
  return OK
end

-- ---------------------------------------------------------------------------
-- PROTOTYPE 2: the batched GUI add
--
-- Two encodings of the same N element specs, so the question "is the win the
-- BATCHING or the ENCODING" has an answer rather than an argument.
--
--   dyn    an array of tier-2 maps, which is what "an array of element specs"
--          means if nothing else changes.
--   typed  a flat block per element -- fixed-width scalars and (ptr,len)
--          strings, laid out the way factorio.Layout already lays out a struct
--          -- plus a parent-index column so one crossing builds a TREE.
-- ---------------------------------------------------------------------------

local DYNW = 16  -- the tier-2 slot width, matching fk_abi's own

local function batchadd_dyn(m, argp, retp)
  local sptr, n, hout = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local made = 0
  for i = 0, n - 1 do
    local spec = H.read_dyn(sptr + i * DYNW)
    local ok, el = pcall(parent.add, spec)
    if ok and el ~= nil then
      st32(hout + i * 4, H.transient(el))
      made = made + 1
    end
  end
  st32(retp, made)
  return OK
end

-- The typed spec's layout. Five fields, the shape a GUI row actually carries.
-- Strings are (ptr, len); the parent column is the tree.
--   0  u32   parent index (0xFFFFFFFF = the receiver)
--   4  u32   type   ptr
--   8  u32   type   len
--  12  u32   name   ptr
--  16  u32   name   len
--  20  u32   caption ptr
--  24  u32   caption len
--  28  u32   style  ptr
--  32  u32   style  len
--  36  u32   tooltip ptr
--  40  u32   tooltip len
--  44  u8    presence bits
local SPECW = 48
local read_string = Mod.read_string

local function batchadd_typed(m, argp, retp)
  local sptr, n, hout = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local made = 0
  local spec = {}
  for i = 0, n - 1 do
    local at = sptr + i * SPECW
    local bits = ld8(at + 44)
    -- A FRESH TABLE PER ELEMENT, because the engine keeps nothing but reads
    -- the table it is handed and a reused one would be measuring a shape no
    -- caller can have.
    spec = {}
    spec.type = read_string(ld32(at + 4), ld32(at + 8))
    if bits % 2 >= 1 then spec.name = read_string(ld32(at + 12), ld32(at + 16)) end
    if bits % 4 >= 2 then spec.caption = read_string(ld32(at + 20), ld32(at + 24)) end
    if bits % 8 >= 4 then spec.style = read_string(ld32(at + 28), ld32(at + 32)) end
    if bits % 16 >= 8 then spec.tooltip = read_string(ld32(at + 36), ld32(at + 40)) end
    local ok, el = pcall(parent.add, spec)
    if ok and el ~= nil then
      st32(hout + i * 4, H.transient(el))
      made = made + 1
    end
  end
  st32(retp, made)
  return OK
end

-- Install the prototypes into the dispatcher the same way a kind would be:
-- one branch, reached from M.call, before the member read.
local realCall = H.call
local members = { [4] = { name = "health", valid = true, fn = nil } }
local protoBulk = bulk_full
local protoBatch = batchadd_dyn
-- Which attribute the bulk arms read. "health" is an f64 and "unit_number" a
-- u32, and the two cost different amounts to STORE -- 187 against 70 ns per
-- element -- so an arm that changed both the shape and the attribute at once
-- would be a measurement of neither.
local protoBulkName = "health"

local function call(h, mid, argp, retp)
  if mid == 4 then
    return protoBulk({ name = protoBulkName, valid = true, retsize = 8,
                       sig = { args = ARG_3U32, rets = RET_F64 } }, argp, retp)
  end
  if mid == 5 then return protoBatch({ name = "add" }, argp, retp) end
  return realCall(h, mid, argp, retp)
end

-- ---------------------------------------------------------------------------
-- The GUI spec corpus, written into guest memory once, outside every timed leg
-- ---------------------------------------------------------------------------

local GUIN = GUICOUNT
local SPECS_DYN = 65536
local SPECS_TYPED = 131072
local STRPOOL = 196608
local HOUT = 245760
-- The pooled encoding: a count, then (ptr,len) pairs, then the bytes; and a
-- spec array whose five string fields are u32 indices into it.
local POOL = 262144 + 65536
local POOLIDX = 262144 + 131072
local PSPECW = 24
local POOLDISTINCT = 0

local function poolPut(s, cursor)
  wstr(cursor, s)
  return cursor + #s
end

-- One row of a table: a flow holding a label and a button. The shape the
-- audited GUI applications repeat 500 to 1,200 times.
local specTable = {}
for i = 1, GUIN do
  specTable[i] = {
    type = (i % 3 == 0) and "button" or ((i % 3 == 1) and "label" or "flow"),
    name = "row-" .. i .. "-cell",
    caption = "Iron plate x" .. (i * 17),
    style = "frame_action_button",
    tooltip = "Click to select this row",
  }
end

for i = 1, GUIN do
  local st = H.write_dyn(SPECS_DYN + (i - 1) * DYNW, specTable[i])
  if st ~= OK then error("write_dyn failed: " .. tostring(st)) end
end

local cursor = STRPOOL
for i = 1, GUIN do
  local s = specTable[i]
  local at = SPECS_TYPED + (i - 1) * SPECW
  st32(at, 0xFFFFFFFF)
  local p = cursor
  cursor = poolPut(s.type, cursor)
  st32(at + 4, p) st32(at + 8, #s.type)
  p = cursor cursor = poolPut(s.name, cursor)
  st32(at + 12, p) st32(at + 16, #s.name)
  p = cursor cursor = poolPut(s.caption, cursor)
  st32(at + 20, p) st32(at + 24, #s.caption)
  p = cursor cursor = poolPut(s.style, cursor)
  st32(at + 28, p) st32(at + 32, #s.style)
  p = cursor cursor = poolPut(s.tooltip, cursor)
  st32(at + 36, p) st32(at + 40, #s.tooltip)
  st8(at + 44, 15)
end

-- The POOLED encoding of the same corpus: one table of distinct strings, and a
-- spec array whose string fields are indices into it. Built here rather than in
-- a leg because every other encoding is built here too, and a leg that built
-- its own input would be timing the build.
do
  local seen, order = {}, {}
  local function intern(s)
    if seen[s] == nil then
      order[#order + 1] = s
      seen[s] = #order - 1
    end
    return seen[s]
  end
  local idx = {}
  for i = 1, GUIN do
    local s = specTable[i]
    idx[i] = { intern(s.type), intern(s.name), intern(s.caption),
               intern(s.style), intern(s.tooltip) }
  end
  st32(POOL, #order)
  local c = POOL + 4 + #order * 8
  for j = 1, #order do
    local s = order[j]
    wstr(c, s)
    st32(POOL + 4 + (j - 1) * 8, c)
    st32(POOL + 8 + (j - 1) * 8, #s)
    c = c + #s
  end
  if c > POOLIDX then error("the pool overran the spec array") end
  for i = 1, GUIN do
    local at = POOLIDX + (i - 1) * PSPECW
    st32(at, 0xFFFFFFFF)
    for k = 1, 5 do st32(at + k * 4, idx[i][k]) end
  end
  POOLDISTINCT = #order
end

-- The per-call add's argument block: one tier-2 map, hoisted. Hoisting is the
-- discipline agents/abi.md's own table uses, and it is FAVOURABLE to the
-- status quo -- a real GUI app rebuilds the map per element, so the unbatched
-- leg below is measured at its best.
H.write_dyn(ARGP, specTable[1])

-- ---------------------------------------------------------------------------
-- The legs
-- ---------------------------------------------------------------------------

local function argsFor(ptr, n, dst)
  st32(ARGP, ptr) st32(ARGP + 4, n) st32(ARGP + 8, dst)
end

local legs = {}

-- The floor: a plain Lua call with an argument and a return, in the same chunk
-- and the same interpreter, doing none of the work under test.
legs.floor = function(reps)
  local function noop(n) return n + 1 end
  local acc = 0
  for _ = 1, reps do acc = noop(acc) end
  if acc < 0 then print(acc) end
end

-- WHAT A HAND-WRITTEN LUA MOD PAYS for the same poll. This is the number the
-- whole 4a question is asked against: not "is the bulk form faster than the
-- per-call form" but "does it get close to what the author would have written".
legs.lua_poll = function(reps)
  local acc = 0.0
  for _ = 1, reps do
    for i = 0, N - 1 do
      local e = ents[i]
      if e.valid then acc = acc + e.health end
    end
  end
  if acc < 0 then print(acc) end
end

-- N separate host calls, the status quo.
legs.percall = function(reps)
  for _ = 1, reps do
    for i = 0, N - 1 do
      call(ld32(HPTR + i * 4), 1, ARGP, RETP)
    end
  end
end

-- ...and the same N calls with the STRING attribute, which is the other
-- attribute shape a polling mod reads.
legs.percall_str = function(reps)
  for _ = 1, reps do
    for i = 0, N - 1 do
      call(ld32(HPTR + i * 4), 2, ARGP, RETP)
    end
  end
end

-- One crossing, every guard kept.
legs.bulk_full = function(reps)
  protoBulk, protoBulkName = bulk_full, "health"
  for _ = 1, reps do
    argsFor(HPTR, N, DSTPTR)
    call(0, 4, ARGP, RETP)
  end
end

-- One crossing, one pcall for the batch.
-- The generic arm, dispatched like every other.
legs.bulk_encode_rets = function(reps)
  protoBulk, protoBulkName = bulk_encode_rets, "health"
  for _ = 1, reps do
    argsFor(HPTR, N, DSTPTR)
    call(0, 4, ARGP, RETP)
  end
end

legs.bulk_batchpcall = function(reps)
  protoBulk, protoBulkName = bulk_batched_pcall, "health"
  for _ = 1, reps do
    argsFor(HPTR, N, DSTPTR)
    call(0, 4, ARGP, RETP)
  end
end

-- One crossing, no guards at all: the floor of the shape.
legs.bulk_bare = function(reps)
  protoBulk, protoBulkName = bulk_bare, "health"
  for _ = 1, reps do
    argsFor(HPTR, N, DSTPTR)
    call(0, 4, ARGP, RETP)
  end
end

-- A bare host call with no argument or return block, N times: the dispatch a
-- batch removes, isolated.
legs.dispatch = function(reps)
  for _ = 1, reps do
    for i = 0, N - 1 do
      call(parentH, 6, ARGP, RETP)
    end
  end
end

-- ---- 4a decomposition: where the 4a per-element cost actually is ----------
--
-- Each leg is the bare bulk loop with ONE step removed, so the difference
-- between two adjacent rows is that step and nothing else.

-- The same loop storing a U32 attribute through st32 instead of an f64 through
-- st_f64. st_f64 is frexp plus two stores; st32 is one. Which of the two a bulk
-- read pays is decided by the ATTRIBUTE, so this is a property of the workload
-- rather than of the design -- and the design has to say so.
legs.bulk_bare_u32 = function(reps)
  for _ = 1, reps do
    for i = 0, N - 1 do
      local obj = H.get(ld32(HPTR + i * 4))
      st32(DSTPTR + i * 4, obj.unit_number)
    end
  end
end

-- Resolve only: read the handle out of guest memory and look it up.
legs.bulk_resolve_only = function(reps)
  local acc = 0
  for _ = 1, reps do
    for i = 0, N - 1 do
      local obj = H.get(ld32(HPTR + i * 4))
      if obj == nil then acc = acc + 1 end
    end
  end
  if acc > 0 then print(acc) end
end

-- Resolve and read the attribute, storing nothing.
legs.bulk_read_only = function(reps)
  local acc = 0.0
  for _ = 1, reps do
    for i = 0, N - 1 do
      local obj = H.get(ld32(HPTR + i * 4))
      acc = acc + obj.health
    end
  end
  if acc < 0 then print(acc) end
end

-- The f64 store on its own.
legs.bulk_store_f64 = function(reps)
  for _ = 1, reps do
    for i = 0, N - 1 do stf64(DSTPTR + i * 8, 100.5) end
  end
end

-- ...and the u32 store on its own.
legs.bulk_store_u32 = function(reps)
  for _ = 1, reps do
    for i = 0, N - 1 do st32(DSTPTR + i * 4, 1234) end
  end
end

-- Reading the handle out of guest memory on its own: the one cost no bulk form
-- can avoid, because the handle array IS the array a search returned.
legs.bulk_ld32_only = function(reps)
  local acc = 0
  for _ = 1, reps do
    for i = 0, N - 1 do acc = acc + ld32(HPTR + i * 4) end
  end
  if acc < 0 then print(acc) end
end

-- THE HOISTED FORM, and it is the lever the decomposition above found.
--
-- `ld32(a)` is a closure into `ld32(MEM, MEMSIZE, a)`, which bounds-checks,
-- tests alignment and selects a shard PER ELEMENT. A bulk read's addresses are
-- a CONTIGUOUS, 4-ALIGNED RUN whose bounds are known before the loop starts, so
-- all three tests hoist -- which is exactly the loop guard the optimizer
-- already applies to a GUEST loop, applied to the host side of one crossing.
--
-- The shard indexing is the emitted `ld32`'s own, read out of mem.lua: a
-- 4-aligned address below 2097152 has its whole word in shard 1.
local MEMT, MEMSZ = Mod.persist.memory()
legs.bulk_direct = function(reps)
  local mem = MEMT
  for _ = 1, reps do
    -- ONE bounds check for the whole span, at the top, exactly as the guard does
    local lo, hi = HPTR, HPTR + N * 4
    local dlo, dhi = DSTPTR, DSTPTR + N * 4
    if lo % 4 ~= 0 or dlo % 4 ~= 0 or hi > 2097152 or dhi > 2097152 then
      error("the hoisted form's precondition does not hold")
    end
    local s = mem[1]
    local hw, dw = lo / 4 + 1, dlo / 4 + 1
    for i = 0, N - 1 do
      local obj = H.get(s[hw + i])
      s[dw + i] = obj.unit_number
    end
  end
end

-- THE SHAPE A REAL IMPLEMENTATION WOULD HAVE, and the one the design proposes.
--
-- Inside fk_abi.lua the two handle tables are upvalues, so resolving a handle
-- is the split-on-a-bit test and one table read INLINE -- no `M.get` call, no
-- second nil test through a function boundary. Everything else is bulk_direct's:
-- one bounds check for the whole span, direct shard indexing, a u32 store.
--
-- The prototype cannot reach fk_abi's own upvalues from out here, so it stands
-- in with a table of the same shape built once. That is faithful to the WORK
-- (one hash lookup per element) and generous by nothing: a real one would look
-- in exactly one table too, because every handle a search returned is transient.
local standInTransient = {}
for i = 0, N - 1 do standInTransient[ld32(HPTR + i * 4)] = ents[i] end

-- IT GOES THROUGH THE DISPATCHER LIKE EVERY OTHER BULK ARM. The first cut of
-- this leg ran the loop directly and reported 66 ns/element at N=1, which is a
-- bulk form that never paid for its crossing -- the exact shape of unfairness
-- the amortization sweep exists to expose. It is one `call()` per crossing now,
-- so the sweep's N=1 row is a real answer.
local function bulk_inline(m, argp, retp)
  local hptr, n, dst = ld32(argp), ld32(argp + 4), ld32(argp + 8)
  local mem, tr = MEMT, standInTransient
  local name = m.name
  -- ONE precondition test for the whole span: 4-aligned, in range, and inside
  -- shard 0. Anything else falls back to the general arm, which is bulk_full.
  if hptr % 4 ~= 0 or dst % 4 ~= 0 or hptr + n * 4 > 2097152
     or dst + n * 4 > 2097152 then
    return bulk_full(m, argp, retp)
  end
  local s = mem[1]
  local hw, dw = hptr / 4 + 1, dst / 4 + 1
  local nok = 0
  for i = 0, n - 1 do
    local obj = tr[s[hw + i]]
    if obj ~= nil and obj.valid ~= false then
      local v = obj[name]
      if v ~= nil then
        s[dw + i] = v
        nok = nok + 1
      end
    end
  end
  st32(retp, nok)
  return OK
end

legs.bulk_direct_inline = function(reps)
  protoBulk, protoBulkName = bulk_inline, "unit_number"
  for _ = 1, reps do
    argsFor(HPTR, N, DSTPTR)
    call(0, 4, ARGP, RETP)
  end
end

-- ...and with the handle RESOLUTION hoisted too, which a bulk form can do
-- because the two handle spaces are split on a bit and a search's results are
-- all transient by construction. This is the floor of the whole shape.
legs.bulk_direct_raw = function(reps)
  local mem = MEMT
  local tr = H.transient_table and H.transient_table() or nil
  for _ = 1, reps do
    local s = mem[1]
    local hw, dw = HPTR / 4 + 1, DSTPTR / 4 + 1
    for i = 0, N - 1 do
      local obj = ents[i]
      s[dw + i] = obj.unit_number
    end
  end
  if tr == nil then return end
end

-- ---- GUI ----

-- What a hand-written Lua GUI builder pays.
legs.lua_add = function(reps)
  for _ = 1, reps do
    for i = 1, GUIN do
      local s = specTable[i]
      parent.add({ type = s.type, name = s.name, caption = s.caption,
                   style = s.style, tooltip = s.tooltip })
    end
  end
end

-- GUIN separate host calls, each carrying a hoisted tier-2 map.
legs.add_percall = function(reps)
  for _ = 1, reps do
    for i = 1, GUIN do
      call(parentH, 3, ARGP, RETP)
    end
  end
end

-- One crossing over an array of tier-2 maps.
legs.add_batch_dyn = function(reps)
  protoBatch = batchadd_dyn
  for _ = 1, reps do
    argsFor(SPECS_DYN, GUIN, HOUT)
    call(parentH, 5, ARGP, RETP)
  end
end

-- One crossing over an array of FLAT specs plus a string pool.
legs.add_batch_typed = function(reps)
  protoBatch = batchadd_typed
  for _ = 1, reps do
    argsFor(SPECS_TYPED, GUIN, HOUT)
    call(parentH, 5, ARGP, RETP)
  end
end

-- The two decodes on their own, with no engine call under them, so the
-- encoding question is separated from the call it feeds.
legs.decode_dyn = function(reps)
  for _ = 1, reps do
    for i = 0, GUIN - 1 do
      local s = H.read_dyn(SPECS_DYN + i * DYNW)
      if s.type == nil then print("bad") end
    end
  end
end

legs.decode_typed = function(reps)
  for _ = 1, reps do
    for i = 0, GUIN - 1 do
      local at = SPECS_TYPED + i * SPECW
      local s = {}
      s.type = read_string(ld32(at + 4), ld32(at + 8))
      s.name = read_string(ld32(at + 12), ld32(at + 16))
      s.caption = read_string(ld32(at + 20), ld32(at + 24))
      s.style = read_string(ld32(at + 28), ld32(at + 32))
      s.tooltip = read_string(ld32(at + 36), ld32(at + 40))
      if s.type == nil then print("bad") end
    end
  end
end

-- ---- 4b decomposition: where the GUI per-element cost actually is ---------

-- The five read_string calls with no table built and no engine call: the
-- irreducible half of the typed decode.
legs.decode_typed_strings = function(reps)
  local n = 0
  for _ = 1, reps do
    for i = 0, GUIN - 1 do
      local at = SPECS_TYPED + i * SPECW
      n = n + #read_string(ld32(at + 4), ld32(at + 8))
              + #read_string(ld32(at + 12), ld32(at + 16))
              + #read_string(ld32(at + 20), ld32(at + 24))
              + #read_string(ld32(at + 28), ld32(at + 32))
              + #read_string(ld32(at + 36), ld32(at + 40))
    end
  end
  if n < 0 then print(n) end
end

-- ...and the table construction with the strings already in hand: the other
-- half. The two together should account for decode_typed.
local prebuilt = {}
legs.decode_typed_table = function(reps)
  for i = 1, GUIN do prebuilt[i] = specTable[i] end
  for _ = 1, reps do
    for i = 1, GUIN do
      local s = prebuilt[i]
      local t = { type = s.type, name = s.name, caption = s.caption,
                  style = s.style, tooltip = s.tooltip }
      if t.type == nil then print("bad") end
    end
  end
end

-- THE POOLED FORM, and it is the one the design turns on. A batch's strings
-- repeat: fifty rows carry fifty distinct names and ONE style, ONE tooltip and
-- three distinct types between them. A string slot that is an INDEX into a
-- per-batch pool decoded once turns 5*G decodes into (distinct strings)
-- decodes -- which is a property of a BATCH and unavailable to a per-call form
-- however it is encoded.
--
-- The pool layout is declared with the other corpus addresses above.
legs.decode_typed_pooled = function(reps)
  local strs = {}
  for _ = 1, reps do
    -- decode the pool once
    local np = ld32(POOL)
    for j = 0, np - 1 do
      strs[j] = read_string(ld32(POOL + 4 + j * 8), ld32(POOL + 8 + j * 8))
    end
    for i = 0, GUIN - 1 do
      local at = POOLIDX + i * PSPECW
      local s = {
        type = strs[ld32(at + 4)],
        name = strs[ld32(at + 8)],
        caption = strs[ld32(at + 12)],
        style = strs[ld32(at + 16)],
        tooltip = strs[ld32(at + 20)],
      }
      if s.type == nil then print("bad") end
    end
  end
end

-- THE CEILING OF STRING REUSE. Every string already decoded, so what is left
-- is the index reads and the table. A per-SESSION intern table -- the guest
-- registers a string once and refers to it by id thereafter -- asymptotes here.
local interned = {}
legs.decode_typed_interned = function(reps)
  local np = ld32(POOL)
  for j = 0, np - 1 do
    interned[j] = read_string(ld32(POOL + 4 + j * 8), ld32(POOL + 8 + j * 8))
  end
  for _ = 1, reps do
    for i = 0, GUIN - 1 do
      local at = POOLIDX + i * PSPECW
      local s = {
        type = interned[ld32(at + 4)],
        name = interned[ld32(at + 8)],
        caption = interned[ld32(at + 12)],
        style = interned[ld32(at + 16)],
        tooltip = interned[ld32(at + 20)],
      }
      if s.type == nil then print("bad") end
    end
  end
end

-- ...and the REALISTIC form: four fields interned, one (the caption, which is
-- the data) freshly decoded every refresh. A GUI refresh changes its numbers
-- and keeps its structure, so this is the shape rather than the ceiling.
legs.decode_typed_mixed = function(reps)
  local np = ld32(POOL)
  for j = 0, np - 1 do
    interned[j] = read_string(ld32(POOL + 4 + j * 8), ld32(POOL + 8 + j * 8))
  end
  for _ = 1, reps do
    for i = 0, GUIN - 1 do
      local at = POOLIDX + i * PSPECW
      local tat = SPECS_TYPED + i * SPECW
      local s = {
        type = interned[ld32(at + 4)],
        name = interned[ld32(at + 8)],
        caption = read_string(ld32(tat + 20), ld32(tat + 24)),
        style = interned[ld32(at + 16)],
        tooltip = interned[ld32(at + 20)],
      }
      if s.type == nil then print("bad") end
    end
  end
end

-- ---------------------------------------------------------------------------
-- Anti-vacuity: every leg must actually have done its work.
--
-- A benchmark that does not assert the work happened measures an error path at
-- full confidence, which is exactly how the first version of this repo's own
-- abicost test reported a string return as cheaper than a no-op.
-- ---------------------------------------------------------------------------

if VERIFY then
  local out = {}
  -- the per-call GET really returns the health
  st32(ARGP, 0)
  local st = call(ld32(HPTR + 3 * 4), 1, ARGP, RETP)
  out[#out + 1] = "percall_status=" .. st .. " v=" .. ldf64(RETP)
  -- the bulk read fills every slot
  protoBulk = bulk_full
  argsFor(HPTR, N, DSTPTR)
  st = call(0, 4, ARGP, RETP)
  out[#out + 1] = "bulk_status=" .. st .. " n=" .. ld32(RETP) ..
                  " first=" .. ldf64(DSTPTR) .. " last=" .. ldf64(DSTPTR + (N - 1) * 8)
  protoBulk = bulk_batched_pcall
  argsFor(HPTR, N, DSTPTR)
  st = call(0, 4, ARGP, RETP)
  out[#out + 1] = "bulkpc_n=" .. ld32(RETP)
  protoBulk = bulk_bare
  argsFor(HPTR, N, DSTPTR)
  st = call(0, 4, ARGP, RETP)
  out[#out + 1] = "bulkbare_n=" .. ld32(RETP)
  -- The hoisted arm writes u32 unit_numbers, so it is checked against those
  -- rather than against health: an arm reading a different attribute than the
  -- one it is compared with is the vacuity this block exists to catch.
  protoBulk, protoBulkName = bulk_inline, "unit_number"
  argsFor(HPTR, N, DSTPTR)
  st = call(0, 4, ARGP, RETP)
  out[#out + 1] = "bulkinline_status=" .. st .. " n=" .. ld32(RETP) ..
                  " first=" .. ld32(DSTPTR) ..
                  " last=" .. ld32(DSTPTR + (N - 1) * 4)
  protoBulkName = "health"
  -- both GUI batches really call add, GUIN times, with a usable spec
  addCount = 0
  protoBatch = batchadd_dyn
  argsFor(SPECS_DYN, GUIN, HOUT)
  st = call(parentH, 5, ARGP, RETP)
  out[#out + 1] = "batchdyn_status=" .. st .. " made=" .. ld32(RETP) .. " adds=" .. addCount
  addCount = 0
  protoBatch = batchadd_typed
  argsFor(SPECS_TYPED, GUIN, HOUT)
  st = call(parentH, 5, ARGP, RETP)
  out[#out + 1] = "batchtyped_status=" .. st .. " made=" .. ld32(RETP) .. " adds=" .. addCount
  -- the two encodings decode to the SAME spec, which is what makes their
  -- timings comparable at all
  local a = H.read_dyn(SPECS_DYN)
  local at = SPECS_TYPED
  local b = {
    type = read_string(ld32(at + 4), ld32(at + 8)),
    name = read_string(ld32(at + 12), ld32(at + 16)),
    caption = read_string(ld32(at + 20), ld32(at + 24)),
    style = read_string(ld32(at + 28), ld32(at + 32)),
    tooltip = read_string(ld32(at + 36), ld32(at + 40)),
  }
  local same = a.type == b.type and a.name == b.name and a.caption == b.caption
               and a.style == b.style and a.tooltip == b.tooltip
  out[#out + 1] = "encodings_agree=" .. tostring(same) ..
                  " type=" .. tostring(b.type) .. " name=" .. tostring(b.name)
  -- the per-call add's hoisted argument is the same spec
  H.write_dyn(ARGP, specTable[1])
  addCount = 0
  st = call(parentH, 3, ARGP, RETP)
  out[#out + 1] = "addpercall_status=" .. st .. " adds=" .. addCount
  print(table.concat(out, "\n"))
  return
end

local fn = legs[LEG]
if fn == nil then error("no such leg: " .. tostring(LEG)) end
fn(REPS)
print("done")
