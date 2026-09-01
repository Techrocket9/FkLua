-- FkLua data-stage ABI: a guest that runs at the SETTINGS and DATA stages.
--
-- `require`d by a packaged mod's settings.lua / data.lua / data-updates.lua /
-- data-final-fixes.lua, which `fklua mod` generates. Hand-written and copied
-- verbatim, like fk_mod.lua and fk_abi.lua.
--
-- ---------------------------------------------------------------------------
-- WHY A SECOND SHIM AT ALL
--
-- fk_mod.lua is the CONTROL stage: it binds the runtime API's handle table,
-- carries the persistence protocol, paces a collector and wires event hooks.
-- None of that exists here. The data stage has no `game`, no `script`, no
-- `storage`, no events and no ticks; it runs once, mutates a table, and the Lua
-- state it ran in is thrown away. A guest that runs here wants exactly one
-- thing -- `data.raw` -- and everything fk_mod.lua does besides would be
-- machinery with nothing behind it.
--
-- So the data guest is a SEPARATE wasm module with a separate entry point, and
-- this is its whole host side. Measured (agents/datastage.md): sharing the
-- control module instead costs a real mod +150 ms of parse per game load, per
-- stage it hooks, for a program the stage never calls.
--
-- ---------------------------------------------------------------------------
-- THE EIGHT IMPORTS
--
--   fkdata.stage()            -> u32     1 settings, 2 data, 3 updates, 4 final-fixes
--   fkdata.get(pathp, retp)   -> status  read data.raw at a path into one tier-2 slot
--   fkdata.set(pathp, valp)   -> status  write; valp == 0 means nil, i.e. delete
--   fkdata.extend(valp)       -> status  data:extend(an array of prototypes)
--   fkdata.clone(pathp, dstp) -> status  deep-copy one data.raw entry to another name
--   fkdata.keys(pathp, retp)  -> status  the keys at a path, SORTED
--   fkdata.env(which, retp)   -> status  1 mods, 2 feature_flags,
--                                        3 settings.startup, 4 the mod's own
--                                        name (packager-supplied; see run),
--                                        5 defines.prototypes
--   fkdata.raise(ptr, len)    -> never   the GUEST'S OWN failure, raised at
--                                        the stage exactly as a host-detected
--                                        one is: the message lands verbatim
--                                        behind fail()'s stage prefix, so it
--                                        carries no stage of its own; the
--                                        call does not return
--
-- plus env.fk_log and env.fk_print, which every guest has.
--
-- A PATH is a tier-2 array of strings and numbers rooted at `data.raw`:
-- {"technology", "logistics", "unit", "count"}. Every argument is a pointer to
-- one tier-2 dynamic value, which is the shape fk.subscribe and fk.remote_call
-- already have -- so the codec is fk_abi.lua's, unchanged, and this file binds
-- only what the codec needs: memory, the allocator and the string scratch
-- region. It does NOT bind globals, the member table or the handle table.
--
-- ---------------------------------------------------------------------------
-- ERRORS RAISE, AND THAT IS A DELIBERATE DEVIATION FROM THE CONTROL ABI
--
-- fk_abi.lua's host calls never raise, for three reasons that do not apply
-- here: there is no wasm frame to unwind through (generated code is plain Lua,
-- but a control-stage failure lands mid-tick), the guest's state has to survive
-- the call, and a lockstep simulation must not diverge. A data-stage failure
-- has none of those. It should stop the load loudly and name the file to fix,
-- which is Factorio's own convention for a broken data stage and is what a mod
-- author wants at load time. So everything here raises, with the STAGE NAME and
-- the OFFENDING PATH in the message.
--
-- The status return is kept in the signature for the one case that is not a
-- failure: a `get` of a key that is not there returns ABSENT (1) rather than
-- raising, because "is this prototype already defined" is a normal question --
-- it is what a mod adopting another mod's entities asks on every load.
--
-- ---------------------------------------------------------------------------
-- DETERMINISM: EVERY TABLE CROSSES SORTED
--
-- The data stage runs per client, and a divergent prototype set is a JOIN
-- REFUSAL rather than a desync -- Factorio checksums the prototype list and
-- turns the client away. Lower stakes than the control stage, and the rule is
-- enforced anyway, because a join refusal nobody can reproduce is worse than a
-- desync that fails loudly.
--
-- `pairs()` over data.raw["transport-belt"] yields INSERTION order
-- (transport-belt, fast, express, turbo), which is deterministic in this Lua
-- and is not a promise this ABI may make. So nothing here hands a guest a
-- pairs() order: write_sorted below is a full recursive mirror of
-- fk_abi.lua's write_dyn whose only difference is that a dictionary's pairs are
-- emitted in sorted key order, at EVERY nesting level, and a key that is
-- neither a number nor a string raises rather than being ordered by something
-- that is not stable.
--
-- That is a property of the CONSTRUCTION rather than a rule a guest has to
-- follow, which is the difference agents/testing.md draws between a refusal and
-- an impossibility.
-- ---------------------------------------------------------------------------

local H = require("fk_abi")
local build = require("fk_data_module")

local M = {}

-- The stage ids, which are the ABI. A guest compiled against these uses the
-- numbers, so appending is safe and renumbering is not.
M.SETTINGS, M.DATA, M.DATA_UPDATES, M.DATA_FINAL_FIXES = 1, 2, 3, 4

-- Stage id -> the name a message says, and the guest export that stage calls.
--
-- KEEP IN STEP WITH internal/factorio.StageHooks. An export listed there that
-- this table does not name would be reported as wired and never called, which
-- is exactly the drift factorio.Hooks had for two milestones -- so the mirror is
-- checked in BOTH directions by TestEveryStageHookIsRegisteredByTheShim and its
-- converse, which read this file.
local STAGE_NAME = { "settings", "data", "data-updates", "data-final-fixes" }
local STAGE_EXPORT = {
  "fk_settings",
  "fk_data",
  "fk_data_updates",
  "fk_data_final_fixes",
}

-- The two statuses. Everything else raises; see the header.
local FKD_OK, FKD_ABSENT = 0, 1
M.OK, M.ABSENT = FKD_OK, FKD_ABSENT

-- How deep a value may nest before this refuses to walk it.
--
-- A prototype is a tree and Factorio's own data.raw has nothing near this deep,
-- so the cap exists to turn a CYCLE into a message instead of a C-stack
-- overflow -- util.table.deepcopy has the same hazard and answers it the same
-- way, by not having cycles to walk.
local MAXDEPTH = 64

-- ---------------------------------------------------------------------------
-- Paths
-- ---------------------------------------------------------------------------

-- Render a path the way a message should show it. Bracket form rather than
-- dotted, because prototype names contain hyphens and `data.raw.transport-belt`
-- is not a thing anyone can type.
local function path_text(path)
  if path == nil or #path == 0 then return "data.raw" end
  local parts = {}
  for i = 1, #path do
    local p = path[i]
    if type(p) == "string" then
      parts[i] = string.format("%q", p)
    else
      parts[i] = tostring(p)
    end
  end
  return "data.raw[" .. table.concat(parts, "][") .. "]"
end

-- ---------------------------------------------------------------------------
-- Sorted encoding
-- ---------------------------------------------------------------------------

-- A total order over table keys. Numbers before strings, each in their own
-- natural order.
--
-- ANYTHING ELSE RAISES, and that is the point rather than an omission: two
-- table keys can only be ordered by their addresses, which differ between runs,
-- so ordering them would produce a per-client order -- the exact thing this
-- sorting exists to prevent. A prototype with a boolean or table key is
-- pathological, and saying so beats a quiet nondeterminism.
local function key_rank(k)
  local t = type(k)
  if t == "number" then return 1 end
  if t == "string" then return 2 end
  return 0
end

local function key_less(a, b)
  local ra, rb = key_rank(a), key_rank(b)
  if ra ~= rb then return ra < rb end
  return a < b
end

-- run(stage, modname). The second argument is the mod's OWN name, written into
-- the generated stage file by `fklua mod` -- the data-stage environment has no
-- "current mod" anywhere, so the packager is the one authoritative source (it
-- is what wrote info.json). Handed back to the guest through env(4). A stage
-- file written by an older fklua passes nothing, which reads as nil rather
-- than raising: the stage files and this shim ship together, so the pair
-- cannot actually skew, and a raise here would turn "cannot happen" into a
-- load failure if it ever did.
function M.run(stage, modname)
  local sname = STAGE_NAME[stage]
  if sname == nil then
    error("fklua: fk_data.run was given stage " .. tostring(stage) ..
          ", and there are four: 1 settings, 2 data, 3 data-updates, " ..
          "4 data-final-fixes", 0)
  end
  if type(modname) ~= "string" then modname = nil end

  local function fail(what)
    error("fklua: at the " .. sname .. " stage, " .. what, 0)
  end

  -- -------------------------------------------------------------------------
  -- The instance.
  --
  -- ONE PER STAGE, BUILT FRESH, AND THAT IS THE DESIGN RATHER THAN A COST.
  -- Measured: the settings stage is its own Lua state, and data, data-updates
  -- and data-final-fixes share one -- but Factorio's `require` re-executes a
  -- file at every stage, so nothing carries across a stage boundary anyway. A
  -- guest therefore keeps NO state between stages, and the place to keep state
  -- between them is data.raw, which is what Factorio's own stages do.
  -- -------------------------------------------------------------------------
  local instance, E, io_

  local function guest_string(ptr, len)
    if not instance.read_string then
      fail("this guest has no linear memory, so a (pointer, length) cannot " ..
           "be followed")
    end
    return instance.read_string(ptr, len)
  end

  -- The guest's own allocator, for the container payloads a tier-2 value needs.
  --
  -- NOTHING IS FREED AND THAT IS CORRECT HERE. A data guest runs once and dies
  -- with the Lua state, so there is no steady state for a leak to grow in; the
  -- control stage's arena bracket exists because a mod ticks for hours.
  local function dalloc(n)
    if n == 0 then return 0 end
    local p = E.fk_alloc(n)
    if p == nil or p == 0 then
      fail("the guest allocator refused " .. tostring(n) .. " bytes")
    end
    return p
  end

  -- write_sorted is write_dyn with one difference: a dictionary's pairs come
  -- out in sorted key order, at every level. See the header.
  --
  -- Scalars delegate to fk_abi.lua's write_dyn, which is the single statement of
  -- the tag numbering and of how a string is written; only the two CONTAINER
  -- branches are restated, because those are the ones with an order in them.
  local write_sorted
  write_sorted = function(at, v, depth, path)
    local vt = type(v)
    if vt == "userdata" or vt == "thread" then
      -- NOT DELEGATED, and this is the one place write_dyn cannot be reused.
      -- Its table branch reaches `#v`, which on userdata without a __len
      -- metamethod raises out of the encode rather than producing a value --
      -- and `helpers` is userdata at both stages. Nothing a guest can do with
      -- one anyway: the handle table is the control stage's and is not bound
      -- here.
      io_.st32(at, H.DYN_NIL)
      return
    end
    if vt ~= "table" then
      -- nil, boolean, number, string, and anything else write_dyn turns into
      -- nil (a function has nowhere to live in a guest).
      local st = H.write_dyn(at, v)
      if st ~= H.OK then
        fail("encoding " .. path_text(path) .. " failed with status " ..
             tostring(st))
      end
      return
    end
    if depth > MAXDEPTH then
      fail(path_text(path) .. " nests more than " .. MAXDEPTH .. " deep; a " ..
           "prototype is a tree, so this is a cycle")
    end

    local n = #v
    local total = 0
    for _ in pairs(v) do total = total + 1 end

    if total == 0 then
      -- An empty table is an empty array; nothing distinguishes the two, and a
      -- guest can tell as much from the count. Same reading write_dyn takes.
      io_.st32(at, H.DYN_ARR)
      io_.st32(at + 8, 0)
      io_.st32(at + 12, 0)
      return
    end

    if n > 0 and n == total then
      local ptr = dalloc(n * H.DYNW)
      for i = 1, n do
        write_sorted(ptr + (i - 1) * H.DYNW, v[i], depth + 1, path)
      end
      io_.st32(at, H.DYN_ARR)
      io_.st32(at + 8, ptr)
      io_.st32(at + 12, n)
      return
    end

    local ks, i = {}, 0
    for k in pairs(v) do
      if key_rank(k) == 0 then
        fail(path_text(path) .. " has a key of type " .. type(k) ..
             "; only string and number keys can be ordered the same way on " ..
             "every client")
      end
      i = i + 1
      ks[i] = k
    end
    table.sort(ks, key_less)

    local ptr = dalloc(total * H.DYNPW)
    for j = 1, total do
      local e = ptr + (j - 1) * H.DYNPW
      write_sorted(e, ks[j], depth + 1, path)
      write_sorted(e + H.DYNW, v[ks[j]], depth + 1, path)
    end
    io_.st32(at, H.DYN_MAP)
    io_.st32(at + 8, ptr)
    io_.st32(at + 12, total)
  end

  -- -------------------------------------------------------------------------
  -- Reading a path out of the guest
  -- -------------------------------------------------------------------------

  local function read_path(p, what)
    local path = H.read_dyn(p)
    if type(path) ~= "table" then
      fail(what .. " was given a path that is not an array")
    end
    for i = 1, #path do
      local t = type(path[i])
      if t ~= "string" and t ~= "number" then
        fail(what .. " was given a path whose element " .. i .. " is a " .. t ..
             "; a path is strings and numbers rooted at data.raw")
      end
    end
    return path
  end

  -- Walk a path. Returns the value and whether every step was there.
  local function resolve(path)
    local v = data and data.raw
    if v == nil then return nil, false end
    for i = 1, #path do
      if type(v) ~= "table" then return nil, false end
      v = v[path[i]]
      if v == nil then return nil, false end
    end
    return v, true
  end

  -- -------------------------------------------------------------------------
  -- The imports
  -- -------------------------------------------------------------------------

  local imports = {
    fkdata = {
      stage = function() return stage end,

      -- Read one value out of data.raw.
      --
      -- ABSENT IS NOT A FAILURE. It is the answer to "has anybody defined this
      -- yet", which is what a mod that adopts an uninstalled neighbour's
      -- prototypes asks on every load, and what a mod probing for another mod's
      -- prototype asks at data-updates.
      get = function(pathp, retp)
        local path = read_path(pathp, "get")
        local v, ok = resolve(path)
        if not ok then
          io_.st32(retp, H.DYN_NIL)
          return FKD_ABSENT
        end
        write_sorted(retp, v, 0, path)
        return FKD_OK
      end,

      -- Write one value into data.raw, at any depth.
      --
      -- valp == 0 IS nil, i.e. DELETE, and it has to be expressible: strip()ing
      -- a cloned prototype is eleven deletions and five assignments, and a
      -- "write false" reading of an absent value would leave eleven fields
      -- present-and-false in the dump.
      --
      -- An intermediate that is not there RAISES rather than being created. A
      -- typo in a prototype name is the overwhelmingly likelier cause than a
      -- deliberate subtree build, and a silently created `data.raw.transprot`
      -- is a mod that loads and does nothing. Build a subtree by setting its
      -- root in one call.
      set = function(pathp, valp)
        local path = read_path(pathp, "set")
        if #path == 0 then
          fail("set was given an empty path; data.raw itself cannot be replaced")
        end
        local parent = data and data.raw
        if parent == nil then
          fail("there is no data.raw at this stage")
        end
        for i = 1, #path - 1 do
          local nxt = parent[path[i]]
          if type(nxt) ~= "table" then
            local prefix = {}
            for j = 1, i do prefix[j] = path[j] end
            fail(path_text(prefix) .. " is " .. type(nxt) .. ", so " ..
                 path_text(path) .. " cannot be set")
          end
          parent = nxt
        end
        local v = nil
        if valp ~= 0 then v = H.read_dyn(valp) end
        parent[path[#path]] = v
        return FKD_OK
      end,

      -- data:extend(protos).
      --
      -- Factorio's own extend is the validator: it checks that every entry has
      -- a type and a name and complains by name when one does not. Nothing here
      -- second-guesses it, because a half-understanding of prototype validity
      -- would refuse things the game accepts.
      extend = function(valp)
        if valp == 0 then
          fail("extend was given no prototypes")
        end
        local v = H.read_dyn(valp)
        if type(v) ~= "table" then
          fail("extend was given a " .. type(v) ..
               "; it takes an ARRAY of prototype tables")
        end
        if #v == 0 and next(v) ~= nil then
          fail("extend was given one prototype rather than an array of them; " ..
               "wrap it")
        end
        if data == nil or data.extend == nil then
          fail("there is no data:extend at this stage")
        end
        data:extend(v)
        return FKD_OK
      end,

      -- Deep-copy one data.raw entry under another name.
      --
      -- THE COPY IS THE ENGINE'S OWN util.table.deepcopy, WHICH IS THE WHOLE
      -- POINT. Reading a prototype into the guest, editing it and extending it
      -- back would re-serialise every leaf through tier 2 -- and any field the
      -- codec cannot express, any float that does not round-trip and any key
      -- the guest's value model drops changes the prototype SILENTLY while the
      -- mod still loads. Under a host-side clone the untouched leaves are
      -- literally the bytes base shipped. Measured on one real mod's four
      -- clones: 999 scalar leaves survive untouched, and the patches that
      -- follow reach about 40 of them.
      --
      -- Both paths are exactly {type, name}: this copies one PROTOTYPE, and the
      -- destination's type may differ from the source's, so cloning across
      -- types needs no second primitive.
      clone = function(pathp, dstp)
        local src = read_path(pathp, "clone")
        local dst = read_path(dstp, "clone")
        if #src ~= 2 or #dst ~= 2 then
          fail("clone takes two paths of exactly {type, name}, and was given " ..
               path_text(src) .. " and " .. path_text(dst))
        end
        local v, ok = resolve(src)
        if not ok then
          fail(path_text(src) .. " is not defined, so there is nothing to clone")
        end
        if type(v) ~= "table" then
          fail(path_text(src) .. " is a " .. type(v) .. ", not a prototype")
        end
        -- REQUIRED LAZILY. `util` is a global at the data stage and is NOT one
        -- at the settings stage, where require("util") still works -- and
        -- requiring it assigns globals as a side effect, so a guest that never
        -- clones should not provoke that.
        local util = require("util")
        local copy = util.table.deepcopy(v)
        copy.type = dst[1]
        copy.name = dst[2]
        if data == nil or data.extend == nil then
          fail("there is no data:extend at this stage")
        end
        data:extend({ copy })
        return FKD_OK
      end,

      -- The keys at a path, SORTED. See the header: this is the deterministic
      -- enumeration primitive, and it is sorted for the same reason everything
      -- else here is.
      keys = function(pathp, retp)
        local path = read_path(pathp, "keys")
        local v, ok = resolve(path)
        if not ok then
          io_.st32(retp, H.DYN_ARR)
          io_.st32(retp + 8, 0)
          io_.st32(retp + 12, 0)
          return FKD_ABSENT
        end
        if type(v) ~= "table" then
          fail(path_text(path) .. " is a " .. type(v) .. ", which has no keys")
        end
        local ks, n = {}, 0
        for k in pairs(v) do
          if key_rank(k) == 0 then
            fail(path_text(path) .. " has a key of type " .. type(k) ..
                 "; only string and number keys can be ordered the same way " ..
                 "on every client")
          end
          n = n + 1
          ks[n] = k
        end
        table.sort(ks, key_less)
        local ptr = dalloc(n * H.DYNW)
        for i = 1, n do
          local st = H.write_dyn(ptr + (i - 1) * H.DYNW, ks[i])
          if st ~= H.OK then
            fail("encoding the keys of " .. path_text(path) ..
                 " failed with status " .. tostring(st))
          end
        end
        io_.st32(retp, H.DYN_ARR)
        io_.st32(retp + 8, ptr)
        io_.st32(retp + 12, n)
        return FKD_OK
      end,

      -- The three things about the ENVIRONMENT a data stage can ask.
      --
      -- Each is flattened into a plain name -> value table here rather than
      -- handed over as the engine's own structure, for two reasons. `mods` and
      -- `feature_flags` are plain tables and would cross fine; `settings.startup`
      -- is a table of {value = ...} wrappers, and unwrapping it on the host is
      -- one line where unwrapping it in two guest languages is two. And
      -- `settings` DOES NOT EXIST at the settings stage -- a mod's own startup
      -- settings are not readable while they are being declared -- so that arm
      -- answers with an empty map rather than raising, which is the honest
      -- answer to "what are the startup settings" at the moment there are none.
      env = function(which, retp)
        local out = {}
        if which == 1 then
          if mods then
            for k, v in pairs(mods) do out[k] = v end
          end
        elseif which == 2 then
          if feature_flags then
            for k, v in pairs(feature_flags) do out[k] = v end
          end
        elseif which == 3 then
          if settings and settings.startup then
            for k, v in pairs(settings.startup) do
              if type(v) == "table" and v.value ~= nil then
                out[k] = v.value
              else
                out[k] = v
              end
            end
          end
        elseif which == 4 then
          -- The mod's OWN name -- the one arm whose answer comes from the
          -- PACKAGER rather than the engine, because the engine has no
          -- "current mod" at these stages. See run(). A string rather than a
          -- map, and nil under a stage file written by an older fklua.
          write_sorted(retp, modname, 0, nil)
          return FKD_OK
        elseif which == 5 then
          -- defines.prototypes: every base prototype type and the concrete
          -- type names that derive from it, which is the map "every kind of
          -- item" needs and data.raw alone cannot answer. The engine keeps it
          -- as base -> { derived -> 0 } with the zeros carrying nothing, so
          -- what crosses is base -> SORTED ARRAY of names -- an array because
          -- the dummy values would be noise, sorted HERE because write_sorted
          -- orders dictionaries and deliberately not arrays. Absent defines
          -- reads as an empty map, the same answer env(3) gives the settings
          -- stage, rather than raising about an engine this shim cannot see.
          if defines and defines.prototypes then
            for base, derived in pairs(defines.prototypes) do
              local names, n = {}, 0
              for d in pairs(derived) do
                n = n + 1
                names[n] = d
              end
              table.sort(names, key_less)
              out[base] = names
            end
          end
        else
          fail("env was asked for " .. tostring(which) ..
               ", and there are five: 1 mods, 2 feature_flags, " ..
               "3 settings.startup, 4 the mod's own name, 5 defines.prototypes")
        end
        write_sorted(retp, out, 0, nil)
        return FKD_OK
      end,

      -- The guest's OWN raise: (ptr, len) of a message, and the call never
      -- returns -- the error unwinds the whole stage, exactly as every
      -- host-detected failure above does, with the stage name prefixed the
      -- same way.
      --
      -- THE ASK CAME FROM A VALIDATION LIBRARY (FkRecipes' dogfood report).
      -- fkdata's failures raise with the stage and the offending path, but a
      -- guest that VALIDATES -- a cycle detector, a presence ladder -- had no
      -- way to put its own diagnostic where the player looks: a guest panic
      -- surfaces as "fklua trap: unreachable", with the carefully built
      -- message surviving only as a log line above it. This is the missing
      -- half of the errors-RAISE convention: the convention now applies to
      -- what the guest can prove, not only to what the host can.
      --
      -- RAW (ptr, len) RATHER THAN A TIER-2 VALUE, deliberately: it is
      -- fk_log's shape, it allocates nothing and decodes nothing, and a
      -- raise path that could itself fail on an allocation would be a
      -- diagnostic that dies of the disease it reports.
      raise = function(ptr, len)
        fail(guest_string(ptr, len))
      end,
    },

    env = {
      -- log() writes to factorio-current.log and works at every stage. There is
      -- no console here and no `game`, so print goes to the same place -- which
      -- is what fk_mod.lua's fk_print already falls back to before the game
      -- exists.
      fk_log = function(ptr, len) log(guest_string(ptr, len)) end,
      fk_print = function(ptr, len) log(guest_string(ptr, len)) end,
    },
  }

  instance = build(imports)
  E = instance.exports
  io_ = instance.memio

  -- Wiring, in the order fk_mod.lua uses and for the same reasons: memory has
  -- to be reachable before anything marshals through it, the allocator is a
  -- GUEST export so it cannot be bound before the module exists, and all of it
  -- has to be in place before _initialize runs, because a guest's package
  -- initialisers can call this ABI.
  if io_ then
    H.bind_memory(io_)
    H.bind_read_string(instance.read_string)
  else
    fail("this guest declares no linear memory, so nothing can cross to it")
  end
  if E.fk_alloc == nil then
    fail("this guest exports no fk_alloc, so a value has nowhere to live in " ..
         "its memory; import the fkdata guest library")
  end
  H.bind_alloc(E.fk_alloc, E.fk_free)

  -- TinyGo builds a reactor: _initialize sets up the heap and runs package
  -- initialisers, and every //go:wasmexport raises `unreachable` until it has.
  if E._initialize then E._initialize() end

  -- AFTER _initialize, not up with the other binds, and the reason is the one
  -- fk_mod.lua records at the same call: these two are //go:wasmexport
  -- functions like any other, so CALLING them any earlier raises out of the
  -- guest's own runtime. bind_alloc above only CAPTURES its references, which
  -- is why it can sit before.
  if E.fk_scratch_base and E.fk_scratch_size then
    H.bind_scratch(E.fk_scratch_base(), E.fk_scratch_size())
  end

  local export = STAGE_EXPORT[stage]
  local fn = E[export]
  if fn == nil then
    fail("this guest exports no " .. export .. ", so there is nothing to run. " ..
         "fklua generates this file only for a stage the guest exports, so " ..
         "either the file or the guest is stale")
  end
  fn()
end

return M
