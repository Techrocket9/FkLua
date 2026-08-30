-- FkLua mod glue. `fklua mod` writes this out as a mod's control.lua.
--
-- Hand-written and copied verbatim: nothing here is generated, so it can be
-- read, edited in a packaged mod, and diffed against this file.
--
-- The generated module ships as fk_module.lua, which returns a FACTORY rather
-- than an instance -- a function taking the host imports table. It has to,
-- because `require` gives a chunk no arguments, and the module reads its
-- imports from the chunk's varargs. Factorio allows `require` only at the top
-- level of control.lua, which is where this is.

local build = require("fk_module")
local H = require("fk_abi")
local API = require("fk_api_gen")

-- ---------------------------------------------------------------------------
-- The host ABI.
--
-- `fk.call` is the whole API surface: one generic dispatch import, with the
-- member table in fk_api_gen.lua saying what each id means. `env.fk_log` and
-- `env.fk_print` remain because a guest wants them whether or not it touches
-- the game.
--
-- Values crossing this boundary obey Invariant A exactly as generated code
-- does: an i32 is an unsigned double in [0, 2^32), an i64 is a (lo, hi) pair.
-- ---------------------------------------------------------------------------

local instance

-- Forward-declared because the imports table closes over `subscribe` before it
-- can be defined, and `subscribe` in turn closes over `dispatch_done`, which
-- needs the persistence state set up further down. Declaring the LOCALS here is
-- what makes those references resolve to these rather than to globals -- a
-- `function subscribe(...)` further down would define a global instead, and the
-- handler would call a nil `dispatch_done` at the first event.
local subscribe, dispatch_done, on_event, arm_deferred, arm_gc
local register_callback, remote_call

-- ---------------------------------------------------------------------------
-- defines, resolved BY NAME at load.
--
-- A define's number is Factorio's own and is not stable across versions, which
-- is why this ABI has never let one be baked into a guest -- and why it CANNOT
-- be: runtime-api.json carries define names and an order, not their values, so
-- there is nothing to bake from. The generated table therefore carries the
-- dotted PATH and this resolves it against the running game, exactly as the
-- event table has always carried defines.events keys.
--
-- Resolved once, here, into a flat array the fk.define import indexes -- so a
-- read never walks a path and never touches a string. A path this Factorio does
-- not have logs once and reads 0, the same "the mod keeps running" direction a
-- missing event takes.
--
-- Zero is also what an id outside this table reads, which is why ids are
-- 1-based: a guest built against a different table gets a diagnosable zero
-- rather than an off-by-one value from a neighbouring group.
local DEFV = {}
do
  local names = API.defines
  if names then
    for id, path in pairs(names) do
      local v = defines
      for part in string.gmatch(path, "[^.]+") do
        v = v and v[part]
      end
      if v == nil then
        log("fklua: this Factorio has no defines." .. path ..
            ", so the guest reads 0 for it. The mod keeps running.")
        DEFV[id] = 0
      else
        DEFV[id] = v
      end
    end
  end
end

-- wasm has no string type, so anything textual arrives as a (pointer, length)
-- into the guest's linear memory.
local function guest_string(ptr, len)
  if not instance.read_string then
    error("fklua: this guest has no linear memory, so a (pointer, length) " ..
          "cannot be followed", 0)
  end
  return instance.read_string(ptr, len)
end

local imports = {
  -- The generic host-call import. ONE import rather than 3283, because a
  -- per-member import that Factorio removed in a point release would be an
  -- unresolved import -- and an unresolved import means the whole module fails
  -- to instantiate. This degrades to ERR_NO_MEMBER on the one call instead.
  fk = {
    call = function(handle, member, argp, retp)
      return H.call(handle, member, argp, retp)
    end,
    -- The same dispatch over a TYPED argument block, for the handful of members
    -- whose parameter table is a discriminated union (LuaGuiElement::add,
    -- LuaSurface::create_entity). Same handle, same member id, same return
    -- block; the argument block is a tier-1 struct plus one optional tier-2
    -- slot for the variant tail instead of one tier-2 map, which is measured at
    -- 3.3x on the host side. A second import rather than a flag on `call`
    -- because the two decode different blocks -- see fk_abi.lua's M.call_typed
    -- and internal/factorio/used.go.
    call_typed = function(handle, member, argp, retp)
      return H.call_typed(handle, member, argp, retp)
    end,
    -- Promote a handle past the end of this dispatch, and give it back.
    retain = function(handle)
      local p = H.retain(handle)
      return p
    end,
    release = function(handle) return H.release(handle) end,
    -- Ask to receive an event. Registered immediately rather than collected
    -- and registered later: script.on_event is legal at load, and a guest that
    -- subscribes during _initialize is doing so at load.
    -- filterp is a pointer to a tier-2 dynamic value, or 0 for unfiltered.
    -- mask is a bitmask of the event's field indices the guest never reads, or
    -- 0 for the whole payload. (namep, namelen) is a NAME to register under
    -- instead of a defines.events number, which is how a CUSTOM INPUT is
    -- addressed -- see subscribe below. A guest compiled before any of the
    -- three existed declares this import with fewer parameters and Lua hands
    -- the rest a nil, which reads the same as 0 -- so no addition here has ever
    -- changed the wire for a mod already in the field.
    subscribe = function(id, filterp, mask, namep, namelen)
      return subscribe(id, filterp, mask, namep, namelen)
    end,
    -- Read a defines.* value, by the per-build id the generated table names.
    -- One array index: the path was resolved at load.
    define = function(id) return DEFV[id] or 0 end,
    -- Ask for fk_on_deferred to be called ONCE on the next tick. See the
    -- deferred-work section below: this is how a guest batches across the many
    -- separate dispatches one tick can deliver.
    defer = function() return arm_deferred() end,
    -- Ask for the guest's collector to be STEPPED until it finishes: arm the
    -- write barrier now, and run one bounded fk_gc_step per tick from a
    -- one-shot on_tick, tearing the registration down again when the collection
    -- ends. Imported only by a guest built --gc=collected, and it is fk.defer
    -- with a different payload -- see the pacing section below.
    gc = function() return arm_gc() end,
    -- THE CALLBACK SEAM, both directions. See "Commands and remote interfaces"
    -- below for the whole design; the short version is that a Lua FUNCTION
    -- cannot cross this boundary in either direction, so the host synthesises
    -- one and dispatches into the guest by id.
    --
    -- register(kind, descp): declare a command (kind 1) or a remote interface
    -- (kind 2). descp points at one tier-2 value describing it, read once here,
    -- exactly the way subscribe reads its filter.
    register = function(kind, descp) return register_callback(kind, descp) end,
    -- remote_call(callp, retp): the outbound half. callp points at one tier-2
    -- ARRAY [interface, method, [args...]] and retp at one tier-2 slot for the
    -- result. Packed rather than passed as parameters because remote.call is
    -- variadic and this import is not allowed to be.
    remote_call = function(callp, retp) return remote_call(callp, retp) end,
    -- last_error(ptr, cap) -> len: WHAT THE ENGINE SAID when the last host call
    -- came back ERR_CALL_FAILED.
    --
    -- A status is an i32 and a message is not, so the two cannot travel back
    -- together. fk_abi.lua has recorded the message since the ABI existed and
    -- nothing carried it across, so a guest could see that the API refused and
    -- never why. Downstream asserts the engine's EXACT refusal text as a
    -- tripwire -- "on_player_mined_entity (ID 76) (76) can't be raised through
    -- script." -- because the day Factorio allows that raise, a check that only
    -- read ok=false would go on passing over a path that had silently become
    -- testable for real.
    --
    -- IT RETURNS THE FULL LENGTH AND COPIES AT MOST cap BYTES, which is the
    -- ordinary two-call shape: a caller with a fixed buffer learns from the
    -- return whether it saw the whole message and can ask again with room.
    -- ZERO means the last call did not fail -- M.call clears the slot on the way
    -- in, so this describes THAT call rather than the session.
    --
    -- A pointer the guest does not own traps, exactly as the guest's own
    -- out-of-bounds store would: fk_wstr checks the whole span before writing a
    -- byte, which is the rule every marshalled string already obeys.
    last_error = function(ptr, cap)
      local s = H.last_error()
      local n = #s
      if n == 0 or ptr == nil or ptr == 0 or cap == nil or cap == 0 then
        return n
      end
      if n > cap then s = s:sub(1, cap) end
      instance.memio.wstr(ptr, s)
      return n
    end,
  },

  env = {
    -- log() writes to factorio-current.log and works everywhere, including
    -- while control.lua is loading and inside on_load.
    fk_log = function(ptr, len)
      log(guest_string(ptr, len))
    end,

    -- game.print() writes to the in-game console, but `game` does not exist
    -- during control.lua load or in on_load. Falling back to the log beats
    -- raising inside a guest's package initialiser, where the error would be
    -- attributed to whatever the guest happened to be doing.
    fk_print = function(ptr, len)
      local s = guest_string(ptr, len)
      if game then game.print(s) else log(s) end
    end,
  },

  -- ---------------------------------------------------------------------
  -- The WASI shim, for a guest built with -target=wasip1.
  --
  -- Three imports, which is the whole surface a TinyGo wasip1 guest needs
  -- unless it opens a file -- and there are no files here. WASI's errno
  -- convention is a return of 0 for success; nothing below can fail in a way
  -- the guest could act on, so they all return 0.
  --
  -- A wasm-unknown guest never reaches any of this: it imports none of them,
  -- and an unbound import is only an error if the module asks for it.
  -- ---------------------------------------------------------------------
  wasi_snapshot_preview1 = {
    -- Go's println, panic output and anything reaching os.Stdout land here as
    -- a scatter-gather write. Both stdout and stderr go to the log, because
    -- the sandbox has neither.
    fd_write = function(fd, iovs, iovs_len, nwritten)
      local io_ = instance.memio
      local parts, total = {}, 0
      for i = 0, iovs_len - 1 do
        local base = io_.ld32(iovs + i * 8)
        local n = io_.ld32(iovs + i * 8 + 4)
        total = total + n
        if n > 0 then parts[#parts + 1] = instance.read_string(base, n) end
      end
      io_.st32(nwritten, total)
      local s = table.concat(parts)
      -- Go writes the trailing newline separately; log() adds its own.
      s = s:gsub("\n$", "")
      if s ~= "" then log(s) end
      return 0
    end,

    -- A guest calling os.Exit inside a tick. There is nothing to exit, so this
    -- is a trap: taking the mod down loudly beats returning and letting the
    -- guest carry on past a point it believed was terminal.
    proc_exit = function(code)
      error("fklua: guest called proc_exit(" .. tostring(code) .. ")", 0)
    end,

    -- DETERMINISM, not entropy. Factorio is a lockstep simulation: every
    -- client must compute identical bytes or the game desyncs, so a real
    -- random source here would be a bug that only shows up in multiplayer.
    --
    -- This is a counter-based PRNG whose state lives in `storage`, which means
    -- it is identical on every client and survives a save. A guest that wants
    -- unpredictability must ask the GAME for it -- LuaRandomGenerator is
    -- seeded from the map and is the only source that is both varied and
    -- synchronised.
    random_get = function(buf, len)
      local io_ = instance.memio
      storage.fk_wasi_rng = (storage.fk_wasi_rng or 0x9E3779B9) % 4294967296
      local x = storage.fk_wasi_rng
      for i = 0, len - 1 do
        -- A linear congruential step, arithmetic rather than bit32: the
        -- emitter's own measured preference, and this runs per byte.
        x = (x * 1103515245 + 12345) % 4294967296
        io_.st8(buf + i, (x - x % 65536) / 65536 % 256)
      end
      storage.fk_wasi_rng = x
      return 0
    end,
  },
}

instance = build(imports)
local E = instance.exports

-- ---------------------------------------------------------------------------
-- Wiring the ABI.
--
-- Order matters and none of it is arbitrary. Memory has to be reachable before
-- any host call can marshal through it; the allocator is a GUEST export so it
-- cannot be bound before the module exists; and all of it has to be in place
-- before _initialize runs, because a guest's package initialisers can call the
-- API.
-- ---------------------------------------------------------------------------

if instance.memio then
  H.bind_memory(instance.memio)
  H.bind_read_string(instance.read_string)
end

-- A string crossing OUT needs guest memory to live in, and only the guest owns
-- that address space. Absent, a member returning a string reports
-- ERR_BAD_ARGS rather than inventing a pointer.
if E.fk_alloc and E.fk_free then
  H.bind_alloc(E.fk_alloc, E.fk_free)
end

-- _G, not a snapshot: `game` does not exist yet and will not until an event
-- fires. The handle table resolves the nine globals on access for exactly that
-- reason.
H.bind_globals(_G)
H.bind_members(API.members)

-- ---------------------------------------------------------------------------
-- Event dispatch.
--
-- Handlers are registered ONLY for what the guest asked for. Registering
-- speculatively is not a small waste: on_tick alone makes Factorio call this
-- mod sixty times a second forever, whether or not the guest wants it.
--
-- The event data is encoded EAGERLY into one scratch buffer, allocated once and
-- reused. Events are flat -- 219 of them averaging 4.8 fields -- so writing the
-- whole struct costs less than a host call per field would, and the buffer
-- means a dispatch allocates nothing.
--
-- defines.events values are Factorio's and are NOT stable across versions, so
-- the generated table carries the NAME and this resolves it here. An event this
-- version does not have is reported and skipped; the mod keeps running.
-- ---------------------------------------------------------------------------

-- FACTORIO ALLOWS ONE HANDLER PER EVENT PER MOD, and script.on_event REPLACES
-- rather than adds. Two things here want on_tick -- the legacy fk_on_tick hook
-- and a guest that subscribes to it through fk.subscribe -- and whichever
-- registered last silently won. The symptom was a subscription that reported
-- success and never fired.
--
-- So registration goes through here: one dispatcher per event, holding a list.
local registered = {}

-- EVENT FILTERS, and why two subscriptions have to agree on one list.
--
-- script.on_event takes ONE filter list per registration and applies it in C++
-- before the handler runs -- which is the whole point of it, since an
-- unfiltered guest pays a dispatch plus a host call plus a string crossing to
-- read entity.name and reject every build event on the map. But this file keeps
-- one dispatcher per event holding a LIST of handlers, so two subscriptions to
-- the same event share a registration and therefore share a filter.
--
-- The only merge that cannot lose an event is the UNION. A filter list is
-- OR-ed term by term (`mode` defaults to "or"), so appending one list to
-- another is exactly the union -- and a subscriber that asked for NO filter
-- widens the registration to unfiltered outright. Erring toward receiving more
-- is the only safe direction: a guest can ignore an event it did not want and
-- cannot act on one it never got.
--
-- `false` means "registered, deliberately unfiltered", which nil cannot say --
-- nil is "no registration yet".
local filters = {}

-- Returns the list to hand Factorio (nil = unfiltered) and whether it moved.
local function merge_filter(which, flt)
  local cur = filters[which]
  if cur == nil then
    filters[which] = flt or false
    return flt, true
  end
  if cur == false then return nil, false end
  if flt == nil then
    filters[which] = false
    return nil, true
  end
  for i = 1, #flt do cur[#cur + 1] = flt[i] end
  return cur, true
end

-- `protect` asks for the FIRST registration to be attempted under pcall and for
-- a failure to be rolled back and reported rather than raised. It exists for one
-- caller: a subscription addressed by NAME, where `which` is a custom-input
-- prototype's name and the engine answers `Unknown event name: <name>` if no
-- such prototype exists -- measured on 2.0.77, and a typo in a guest must not
-- take the whole mod down at load.
--
-- OFF FOR A NUMERIC id, and that is the point of the parameter rather than
-- protecting unconditionally. A numeric registration cannot raise on the id (it
-- came out of defines.events, so the engine has it), and if it ever did, failing
-- loudly is the right answer where silently not registering is not. Returns
-- whether the event is registered.
function on_event(which, fn, flt, protect)
  local use, moved = merge_filter(which, flt)
  local list = registered[which]
  if list then
    list[#list + 1] = fn
    -- set_event_filter rather than re-registering: the dispatcher is already
    -- correct and only the filter moved. It raises on an event that takes no
    -- filters, which a guest can ask for by mistake, and taking the whole mod
    -- down at load for that is worse than running unfiltered and saying so.
    if moved and script.set_event_filter then
      local ok = pcall(script.set_event_filter, which, use)
      if not ok then
        filters[which] = false
        pcall(script.set_event_filter, which, nil)
        log("fklua: this event takes no filters, so the guest's list was " ..
            "dropped and it will be entered for every one of them.")
      end
    end
    return true
  end
  list = { fn }
  registered[which] = list
  local handler = function(e)
    -- A handler may unregister ITSELF while this is running -- the deferred
    -- flush below does exactly that -- so the walk is by identity rather than
    -- by a count taken up front. `for i = 1, #list` evaluates #list once, so a
    -- removal makes its last iteration index a nil and call it; advancing only
    -- when the slot still holds the function that just ran handles both a
    -- removal (do not advance, the neighbour shifted down into this slot) and
    -- a re-arm (advance, the same function is back where it was).
    local i = 1
    while true do
      local fn_ = list[i]
      if fn_ == nil then break end
      fn_(e)
      if list[i] == fn_ then i = i + 1 end
    end
  end
  if use == nil then
    if protect then
      -- ROLLED BACK ON FAILURE, and the rollback is the part that is not
      -- obvious: `registered[which]` and `filters[which]` are already set, so
      -- leaving them behind would make a later subscription to the same name
      -- append to a list Factorio never registered a dispatcher for -- silently,
      -- because the second call takes the `if list` arm above and returns
      -- success.
      local ok, err = pcall(script.on_event, which, handler)
      if not ok then
        registered[which] = nil
        filters[which] = nil
        log("fklua: script.on_event refused the event name " ..
            tostring(which) .. ": " .. tostring(err) ..
            ". The guest will not receive it. The mod keeps running.")
        return false
      end
    else
      script.on_event(which, handler)
    end
  elseif not pcall(script.on_event, which, handler, use) then
    -- Same reasoning as above: an event that takes no filters is a mistake in
    -- the guest, and running unfiltered with a line in the log beats failing to
    -- load at all.
    filters[which] = false
    script.on_event(which, handler)
    log("fklua: this event takes no filters, so the guest's list was dropped " ..
        "and it will be entered for every one of them.")
  end
  return true
end

-- The inverse, and the reason `registered` holds a list rather than a function:
-- an empty list means nothing wants this event any more, so the dispatcher is
-- torn down and Factorio stops calling into the mod for it. That is what makes
-- a one-shot on_tick cost nothing in steady state instead of being a per-tick
-- call into a handler that returns immediately.
local function off_event(which, fn)
  local list = registered[which]
  if list == nil then return end
  for i = 1, #list do
    if list[i] == fn then
      table.remove(list, i)
      break
    end
  end
  if #list == 0 then
    registered[which] = nil
    filters[which] = nil
    script.on_event(which, nil)
  end
end

-- DISPATCH NESTS, because Factorio raises some events synchronously from inside
-- the API call that caused them -- create_entity{raise_built=true} and
-- entity.die() are the everyday ones. So a guest handler that calls the API can
-- be re-entered before it returns, and everything a dispatch owns has to
-- survive that.
--
-- Two things did not. The scratch buffer was a single address, so the inner
-- dispatch encoded its event over the outer one's, and a handler reads its
-- fields lazily from the pointer it was handed -- it does not copy them out
-- first. And dispatch_done released the whole transient handle space, so an
-- entity the OUTER event handed the guest stopped resolving halfway through the
-- handler. Worse than stopping: clear_transient restarts the id counter, so the
-- outer handle could come back pointing at a different object, which in a
-- lockstep game is a desync rather than an error.
--
-- Depth fixes both. One buffer per level, and the end-of-dispatch work happens
-- when the OUTERMOST call returns.
local depth = 0

-- The scratch buffers are allocated LAZILY, at the first dispatch that needs
-- one.
--
-- Not at load: fk_alloc is a guest export, and TinyGo raises
-- "//go:wasmexport function called before runtime initialization" for any
-- export called before _initialize. Not in subscribe either, because a guest
-- subscribes FROM _initialize -- calling back into another export while the
-- runtime is still starting is not something to rely on. By the first event
-- everything is up.
--
-- Per level rather than one, and still one allocation for a mod that never
-- nests: level 2 is only ever allocated by a guest that actually re-enters.
--
-- THROUGH fk_alloc_static, NOT fk_alloc, AND THAT IS LOAD-BEARING. These
-- buffers are cached for the life of the session, and fk_alloc now hands out
-- ARENA memory that the calling binding reclaims when it returns. Every level
-- past the first is allocated from inside a NESTED dispatch -- that is, while
-- an outer binding's bracket is open -- so an arena buffer here would be given
-- away the moment that binding returned, while this table went on writing event
-- data into it. fk_alloc_static is the allocation that outlives its call.
--
-- AND THE CACHE IS MIRRORED INTO `storage`, BECAUSE A LUA LOCAL IS NOT
-- LOAD-STABLE AND THE GUEST HEAP IS. That asymmetry is the whole defect: every
-- load re-executes control.lua, so this table comes back EMPTY, while the heap
-- comes back from the save with the buffers already sitting in it. The first
-- dispatch after a load therefore allocated a SECOND buffer beside one that was
-- already there -- event_scratch bytes of guest heap per level per load, one
-- more entry pinned in the guest's `kept` list, and every allocation the loaded
-- instance makes afterwards landing that much further up than on an instance
-- that never reloaded. On an ordinary single-player load that is a permanent
-- per-load leak into the save. On a MULTIPLAYER JOIN it is two peers whose bump
-- pointers disagree for the rest of the game, which is exactly the shape
-- CLAUDE.md's "no peer-local signal may mutate guest state" rule exists to
-- forbid: `script.on_load` runs on the joining client and on nobody else.
--
-- So the cache is the same kind of thing storage.fk_handles is: a LIVE table
-- aliased into `storage`, published on a fresh heap by state_init and adopted
-- back by state_load under the same_build() gate -- because a pointer is only a
-- pointer into the heap laid out by the build that made it.
--
-- publish_buffers() is called HERE, at the allocation, and not only from
-- state_init, for two reasons. It is what makes a save written by an OLDER
-- runtime heal on its first load rather than never -- the build stamp is a hash
-- of the guest wasm alone, so upgrading FkLua and repackaging leaves
-- same_build() true over a save that carries no mirror, and nothing else would
-- ever put one there. And this is the one place the write is legal: an
-- allocation that already mutates storage.fk_mem is on a replicated path by
-- construction, so a `storage` write beside it adds no peer-local branch that
-- the allocation had not already taken. A write from on_load would.
local publish_buffers
local scratch = {}
local function event_buffer(level)
  if not (API.event_scratch and API.event_scratch > 0 and E.fk_alloc_static) then return 0 end
  local buf = scratch[level]
  if buf == nil then
    buf = E.fk_alloc_static(API.event_scratch)
    scratch[level] = buf
    publish_buffers()
  end
  return buf
end

-- THE MARSHALLING ARENA'S OUTERMOST BRACKET.
--
-- fk_alloc hands out ARENA memory, and every GUEST-initiated call gives it back:
-- the generated binding takes a mark before the call and releases it after, which
-- is why TestAHostCallKeepsNoHeap reads 0 B/call. A HOST-initiated dispatch has
-- no binding to do that, because nothing on the guest side made the call -- so a
-- payload too large for the 4 KiB string scratch region fell back to fk_alloc and
-- advanced the bump pointer by its own size, per event, for the session. Under
-- -gc=leaking that is a leak into every save; under --gc=collected it is worse
-- rather than better, because the arena's chunks are rooted and the collector
-- cannot reclaim one. Nothing in this repo's corpus carries a large string in an
-- event, which is the whole of why it went four milestones unseen.
--
-- So the OUTERMOST dispatch takes the bracket. That is the same boundary the
-- string scratch region already resets at, for the same reason: at depth 0
-- nothing the host wrote is still being read. What makes it sound rather than
-- merely convenient is that everything crossing inbound is COPIED OUT by the
-- generated decoders before the handler returns -- a string becomes a Go string,
-- an array a Go slice -- which is CLAUDE.md's safe-point precondition read in the
-- other direction: a (ptr, len) is consumed before the call that produced it
-- returns.
--
-- BOTH EXPORTS ARE OPTIONAL AND THAT IS LOAD-BEARING. A guest built against an
-- older substrate exports neither and gets precisely the behaviour it had, leak
-- and all. Requiring them would turn every mod already packaged into one that
-- stops loading, which is a much larger failure than the one being fixed -- the
-- same reasoning bind_alloc and bind_scratch are already written with.
--
-- WHAT IT COSTS: +366 ns per OUTERMOST dispatch under --persist=table and
-- +357 under packed, paired, which is the two guest calls and nothing else --
-- every leg that allocates a block is flat in both modes. The two columns
-- agreeing is what says so, and it is not free by accident: the guest side skips
-- the store when it has nothing new to say, because a package-level var is
-- LINEAR MEMORY and a page nothing else dirtied is ~40 µs of repack under
-- packed. Unguarded, a do-nothing dispatch measured 614 ns -> 51.4 µs.
-- Bound here rather than after _initialize, unlike bind_scratch below it, and
-- the difference is that this CAPTURES a reference where that one CALLS. A
-- //go:wasmexport raises `unreachable` until _initialize has run, and the first
-- outermost dispatch is by construction later than that -- _initialize is invoked
-- directly, not through dispatch, precisely so that nothing here runs first.
local arena_mark, arena_release
if E.fk_arena_mark and E.fk_arena_release then
  arena_mark, arena_release = E.fk_arena_mark, E.fk_arena_release
end
local arena_tok = nil

-- A LOAD THAT DECLINED THE SAVED HEAP HAS UNFINISHED BUSINESS, AND THIS IS WHERE
-- IT IS FINISHED. The decision is taken in state_load, which runs from on_load
-- and therefore may not write `storage` -- so it records the fact here and the
-- first REPLICATED execution point after the load acts on it. Everything about
-- why that is the shape, and why the first outermost dispatch is the point, is
-- in finish_rebuild's own header down in the persistence section; the flag lives
-- up here because this is the function that reads it.
--
-- The steady-state cost is one boolean upvalue read per OUTERMOST dispatch,
-- against the alternative of a permanent on_tick registration. The flag can only
-- become true in state_load, which is defined long after finish_rebuild is, so
-- there is no window in which the branch is taken and the function is still nil.
local rebuild_pending = false
local finish_rebuild

-- The two halves of an outermost dispatch boundary, so the three entry paths
-- that have one -- dispatch, the event closure through it, and the callback
-- trampoline, which does its own depth bookkeeping -- cannot disagree about what
-- it consists of. They disagreed about the scratch reset once already.
local function enter_outermost()
  -- BEFORE the reset and the mark, not after, and that ordering is load-bearing
  -- rather than tidy. finish_rebuild dispatches fk_migrate through `dispatch`,
  -- which is itself an outermost dispatch: it takes its own arena mark and gives
  -- it back. Run after the mark below, it would OVERWRITE arena_tok with its own
  -- and then release that one, leaving this dispatch's mark unreachable and the
  -- arena permanently above where it started. Run first, the nested dispatch
  -- opens and closes entirely before this one begins.
  if rebuild_pending then finish_rebuild() end
  H.scratch_reset()
  if arena_mark then arena_tok = arena_mark() end
end

-- The arena goes back BEFORE dispatch_done rather than after, and the ordering is
-- the pcall's argument applied to a second piece of state: nothing in
-- dispatch_done reads guest memory the host wrote -- it releases handles and
-- copies globals and memory size back out -- and doing it first means a raise in
-- there cannot strand the arena at a mark nothing will ever release again.
local function leave_outermost()
  if arena_release and arena_tok ~= nil then
    arena_release(arena_tok)
    arena_tok = nil
  end
  dispatch_done()
end

-- Run one guest entry point and close out the dispatch it belongs to.
--
-- The pcall is not decoration. A trap must not leave `depth` raised -- that
-- would strand every later dispatch inside a nesting that has ended, and no
-- transient handle would ever be released again -- and it is also what makes
-- clear_transient's promise true, that a guest which trapped does not keep its
-- handles. The error is re-raised unchanged at level 0, so what Factorio
-- reports is exactly what it reported before.
-- It returns the callee's first result, which every caller but the collector's
-- step ignores. fk_gc_step reports the phase it left the collector in, and the
-- host reads it to decide whether the barrier stays armed and whether another
-- step is scheduled -- so the value has to come back out through the pcall
-- rather than through a second export the host would have to call separately.
local function dispatch(fn, ...)
  -- The OUTERMOST dispatch is the only point at which nothing the host wrote
  -- into the string scratch is still being read, so it is the only point the
  -- region may go back to zero. Inside a dispatch the region is reclaimed per
  -- host call, back to that call's own mark and never further -- see
  -- scratch_release in fk_abi.lua. Resetting here on every dispatch instead
  -- would hand a nested handler's return values the same bytes an outer
  -- handler is still reading its event fields out of.
  --
  -- Before the increment, so a re-entrant dispatch does not reset.
  if depth == 0 then enter_outermost() end
  depth = depth + 1
  local ok, r = true, nil
  if fn then ok, r = pcall(fn, ...) end
  depth = depth - 1
  if depth == 0 then leave_outermost() end
  if not ok then error(r, 0) end
  return r
end

-- Whether a dispatch is in progress. The collector's safe-point precondition is
-- stated in terms of this and nothing else, so it is exported from the dispatch
-- machinery rather than re-derived where it is asserted.
local function dispatch_depth() return depth end

-- filterp is a pointer to a tier-2 dynamic value in GUEST memory, or 0.
--
-- The codec already carries this shape exactly -- Factorio's filter list is an
-- array of string-keyed tables, which is DYN_ARR of DYN_MAP -- so nothing new
-- crosses the boundary and the guest builds it with the same writeDyn every
-- tier-2 argument goes through. Reading it here rather than baking it into the
-- generated table is what makes a filter a VALUE the guest computes: the
-- prototype names a mod filters on are its own, and no amount of scanning
-- runtime-api.json would ever find them.
--
-- Decoded ONCE, at subscribe time, which happens during _initialize -- so this
-- is a load-time cost and never a per-event one, which is the entire point.
--
-- `mask` is the same idea one step further in: a bitmask over the event's field
-- indices saying which fields the guest never reads, resolved into a field list
-- ONCE here rather than tested per field per dispatch. It exists because the
-- encode is eager and complete -- on_undo_applied deep-copies every
-- BlueprintEntity in `actions` before a handler that wants one uint32 is
-- entered -- and because the two alternatives do not survive contact with the
-- layout: pruning by what the generated readers touch cannot see a guest
-- reading at a hand offset, and a lazy per-field host call is a new import plus
-- the re-entrancy rule that already bit the scratch buffer twice.
--
-- Only OPTIONAL and CONTAINER fields are maskable, so a masked field reads as
-- absent or as empty and never as a plausible zero; H.mask_fields is where that
-- is enforced and a refused bit is logged rather than fatal. Mask 0 is exactly
-- today's behaviour, which is what an old guest sends by not sending anything.
--
-- (namep, namelen) IS A REGISTRATION KEY, AND IT IS WHAT MAKES A CUSTOM INPUT
-- REACHABLE AT ALL. `LuaEventType` is a union of four arms and this import
-- reached exactly one of them, the described `defines.events` set, through a
-- dense index. A CUSTOM INPUT is name-addressed -- `script.on_event("my-input",
-- f)` -- and has NO defines.events entry, measured: `defines.events
-- .CustomInputEvent` is nil on 2.0.77 while the table holds 233 other keys. The
-- trap was that the description carries CustomInputEvent as an ordinary event,
-- so the generator emitted a complete binding -- id constant, payload struct,
-- reader, three field masks -- and a guest that found the right constant was
-- told this Factorio has no such event, which is a falsehood about the one
-- mistake it made.
--
-- THE ID STAYS AN i32 CONSTANT IN ITS OWN OPERAND, which is the whole reason
-- this is a widening of `subscribe` rather than a third `fk.register` kind.
-- `fklua mod` prunes the packaged event table by scanning the wasm for a
-- constant reaching operand 0, so a register DESCRIPTOR -- a tier-2 blob -- would
-- prune the payload descriptor out of the very mod that needs it. The id
-- supplies the LAYOUT and the name supplies the KEY, and neither is the other's
-- business.
--
-- SEVERAL CUSTOM INPUTS CAN SHARE ONE GUEST REGISTRATION and disambiguate on
-- the payload's own `input_name`, because every one of them encodes through the
-- same CustomInputEvent descriptor. The guest sees one id and reads the field.
--
-- A RAW (ptr, len) RATHER THAN A TIER-2 STRING, which is `fk_log`'s and
-- `fk.last_error`'s shape and not `filterp`'s. It costs the guest no allocation
-- and no write_dyn, which keeps the wrapper small enough that the constant scan
-- keeps inlining it -- the R6 defect, where a wrapper that grew stopped being
-- inlined and every mod using it silently shipped all 219 event descriptors. The
-- bytes are read INSIDE this call, which is the standing rule for a (ptr, len) a
-- guest hands the host.
function subscribe(id, filterp, mask, namep, namelen)
  local ev = API.events and API.events[id]
  if ev == nil then return H.ERR_NO_MEMBER end
  if not E.fk_on_event then return H.ERR_NO_MEMBER end

  local which, named
  if namep and namep ~= 0 and namelen and namelen > 0 then
    which = guest_string(namep, namelen)
    named = true
  else
    which = defines.events[ev.name]
    if which == nil then
      -- TWO CAUSES, ONE SYMPTOM, and the message names both because the second
      -- one is the likelier and used to be misdiagnosed as the first. An event
      -- can be absent because this Factorio does not have it, or because it is
      -- not addressed by defines.events at all.
      log("fklua: fk.subscribe could not resolve defines.events." .. ev.name ..
          " -- either this Factorio has no such event, or the event is " ..
          "addressed by NAME. A custom input is delivered to its own prototype " ..
          "name and has no defines.events entry, so subscribe to it with the " ..
          "name form (SubscribeNamed in Go, subscribe_named in Rust). The mod " ..
          "keeps running.")
      return H.ERR_NO_MEMBER
    end
  end

  -- A malformed filter must not take the mod down at load. Unfiltered is the
  -- widening direction, which is the one that cannot lose an event.
  local flt
  if filterp and filterp ~= 0 then
    local ok, v = pcall(H.read_dyn, filterp)
    if ok and type(v) == "table" and #v > 0 then
      flt = v
    else
      log("fklua: the filter passed to fk.subscribe for " .. ev.name ..
          " could not be read, so the guest will be entered for every one.")
    end
  end

  -- Resolved once. Two subscriptions to one event keep their own field lists,
  -- because each closure captures its own -- unlike the filters above, which
  -- share a registration and therefore have to be merged. Nothing has to be
  -- merged here: a mask only says what THIS handler will not look at.
  local fields = ev.fields
  if mask and mask ~= 0 then
    local refused
    fields, refused = H.mask_fields(ev.fields, mask)
    if #refused > 0 then
      log("fklua: fk.subscribe asked to skip " .. table.concat(refused, ", ") ..
          " of " .. ev.name .. ", which are mandatory non-container fields. " ..
          "They are encoded anyway -- a zero there is one the guest could not " ..
          "tell from a real value.")
    end
  end

  -- THE ENCODE HAPPENS INSIDE THE DISPATCH, and that is a fix rather than a
  -- rearrangement. It used to run in the closure below, before `dispatch` was
  -- entered -- so the payload's string fields were written into the scratch
  -- region and `dispatch` then reset that region out from under them, and the
  -- arena mark this now takes would have been taken after the one allocation it
  -- exists to reclaim. run_callback's header has said for a milestone that
  -- dispatch's reset is "correct for an event, whose payload it encodes AFTER
  -- raising the depth"; that sentence describes this shape and described nothing
  -- before it.
  --
  -- What it buys: the payload's strings are live for the whole handler, which is
  -- what a handler reading its fields lazily has always been promised, and the
  -- fallback allocation for one too large to fit is inside the bracket.
  --
  -- depth is already raised when this runs, so event_buffer(depth) names the same
  -- per-level buffer the old event_buffer(depth + 1) did.
  local function run_event(e)
    local buf = event_buffer(depth)
    if buf == 0 and ev.size > 0 then return end
    H.write_struct(fields, buf, e)
    return E.fk_on_event(id, buf)
  end

  if not on_event(which, function(e) dispatch(run_event, e) end, flt, named) then
    -- Only a NAMED registration can get here: `protect` is false for a numeric
    -- id, so on_event returns true or raises. The name did not name a
    -- custom-input prototype this game has, on_event has said so by name and
    -- rolled its own state back, and the guest is told with the status a
    -- subscription to something absent has always used.
    return H.ERR_NO_MEMBER
  end
  return H.OK
end

-- ---------------------------------------------------------------------------
-- Commands and remote interfaces -- the callback seam.
--
-- THE PROBLEM, stated exactly. Three members of the API are unbindable and the
-- generator records why: `LuaCommandProcessor::add_command` and
-- `LuaRemote::add_interface` take a Lua FUNCTION, and `LuaRemote::call` is
-- variadic. `fk_abi.lua`'s write_dyn is blunt about the first half -- "a
-- function crossing into a guest has nowhere to live" -- and it is right. A
-- wasm guest has no callable Lua value and no way to make one.
--
-- THE ANSWER IS THAT THE FUNCTION DOES NOT CROSS. The host synthesises a Lua
-- closure, gives THAT to Factorio, and the closure dispatches into the guest by
-- an id the guest chose. Which is exactly what `subscribe` already does for
-- events -- the closure it installs at the bottom of that function is a
-- trampoline in every sense -- so this is one mechanism generalised rather than
-- a new one, and it is deliberately built out of the same four parts:
--
--   registration   one host call at load, carrying a tier-2 DESCRIPTOR read
--                  once (subscribe reads its filter the same way)
--   the closure    installed by the host, capturing an id and nothing else
--   the entry      ONE guest export, id-dispatched, like fk_on_event
--   the buffer     per nesting level, allocated through fk_alloc_static
--
-- WHY THE GUEST DECLARES THESE AND `fklua.toml` DOES NOT. The manifest was the
-- obvious home -- it already carries identity, dependencies and the data stage,
-- and a static list would let the host register without asking the guest
-- anything. It is the wrong home for two reasons, and the second is decisive.
-- First, the names would then live in two places: the manifest and the guest's
-- own id switch, which is the "configs written twice" complaint fklua-ports
-- already recorded against the data stage (its finding Q5). Second, and this is
-- the one that settles it: a command registration is NOT SAVED. Factorio
-- re-executes control.lua on every load, so registration must happen on every
-- load, and a guest that registers from its own `_initialize` gets that for
-- free -- with no `storage` flag, no on_load re-arm, and no way for the two to
-- disagree. `fk.defer` needs `storage.fk_deferred` precisely because it is
-- arming something that must survive a save; this is the opposite case, and
-- treating it like the same one would be the bug.
--
-- WHY ONE EXPORT AND NOT TWO. A command handler and a remote method differ in
-- what Factorio does with the result and in nothing else the ABI can see, so
-- they share `fk_on_call(id, argp, retp) -> status`: `argp` is one tier-2 ARRAY
-- of the arguments as they arrived, `retp` is one tier-2 slot the guest writes,
-- and a command's trampoline simply ignores what is in it. The guest's id space
-- is the guest's own -- the host stores nothing but the closure that captures
-- one, so there is no table here to keep in step with anything.
--
-- WHY THE ARGUMENTS ARE TIER-2 RATHER THAN A GENERATED STRUCT. A command
-- handler is handed a `CustomCommandData` table, which IS a described concept
-- and could have had a struct like an event payload. A remote method is handed
-- whatever the calling mod passed, which cannot: there is no description of it
-- anywhere, because the other end is another mod. One shape that serves both is
-- worth more than a typed shape that serves one, and `H.write_varargs` is
-- honest about arity in a way `{...}` is not -- see its header.

-- The tier-2 slots a trampoline needs, per nesting level.
--
-- Two slots: the arguments and the result. Through fk_alloc_static and cached
-- per level for exactly the reasons event_buffer's header gives -- an arena
-- buffer allocated inside a nested dispatch is handed back the moment the outer
-- binding returns, while this table is still writing into it.
--
-- AND MIRRORED FOR THE OTHER REASON event_buffer's header gives, because this
-- is the same cache one key over. A command registration is remade on every
-- load (that is why it lives in the guest's _initialize and not in fklua.toml),
-- but the BUFFER it dispatches through is heap, and heap survives -- so a
-- loaded instance that took a callback allocated a second pair of slots beside
-- the pair already in the save, exactly as the event buffer did. It is the
-- smaller leak of the two and it is the same one; a fix that took only the
-- event buffer would be this repo's own recorded failure shape, a guard written
-- for the first instance of a pattern that does not generalise to the second.
local callbuf = {}
local function call_buffer(level)
  if not E.fk_alloc_static then return 0 end
  local buf = callbuf[level]
  if buf == nil then
    buf = E.fk_alloc_static(H.DYNW * 2)
    callbuf[level] = buf
    publish_buffers()
  end
  return buf
end

-- Run one guest callback and return what it wrote, or nil.
--
-- THE DEPTH BOOKKEEPING IS DONE HERE RATHER THAN LEFT TO dispatch, and that is a
-- fix rather than a flourish. `dispatch` calls H.scratch_reset() when it finds
-- depth == 0 -- correct for an event, whose payload it encodes AFTER raising the
-- depth. A trampoline cannot work that way: it has to encode the arguments
-- BEFORE it knows which export to call them with, so by the time dispatch ran
-- they were already in the region it was about to zero. What that produced was
-- not an error: the outer invocation's arguments read back correctly on the
-- first look, and were overwritten by the first NESTED callback, so a command
-- handler that called remote.call and then looked at its own arguments again saw
-- somebody else's. Exactly the shape of the re-entrancy defect
-- TestANestedDispatchLeavesTheOuterOneIntact was written for.
--
-- So the reset happens once, explicitly, at the top of an outermost invocation,
-- and depth is raised across the encode so the inner dispatch cannot repeat it.
--
-- THE SCRATCH MARK IS THE SECOND HALF and is about nesting rather than resetting.
-- Container payloads inside a tier-2 value come from the region (fk_abi.lua's
-- dyn_alloc), so a callback invoked from inside another one -- a remote method
-- called by a guest that is already running inside a command handler -- must
-- take its arguments from ABOVE whatever is still being read, and give the space
-- back when it returns.
local function run_callback(id, ...)
  local buf = call_buffer(depth)
  if buf == 0 then
    error("fklua: this guest exports no fk_alloc_static, so it cannot receive " ..
      "callback arguments", 0)
  end
  local mark = H.scratch_mark()
  local st = H.write_varargs(buf, ...)
  if st ~= H.OK then
    H.scratch_release(mark)
    error("fklua: could not encode the arguments of callback " .. tostring(id) ..
      " (status " .. tostring(st) .. ")", 0)
  end
  -- The result slot is cleared rather than left alone: the buffer is reused
  -- across invocations, so a guest that writes nothing must read as nil rather
  -- than as whatever the previous call returned. The same reasoning as the
  -- masked-field zeroing in write_struct.
  H.write_dyn(buf + H.DYNW, nil)
  local rst = E.fk_on_call(id, buf, buf + H.DYNW)
  local ret = nil
  if rst == nil or rst == H.OK then
    ret = H.read_dyn(buf + H.DYNW)
  end
  H.scratch_release(mark)
  if rst ~= nil and rst ~= H.OK then
    error("fklua: callback " .. tostring(id) .. " returned status " ..
      tostring(rst), 0)
  end
  return ret
end

local function invoke_callback(id, ...)
  local outermost = depth == 0
  if outermost then enter_outermost() end
  depth = depth + 1
  local ok, r = pcall(run_callback, id, ...)
  depth = depth - 1
  -- After run_callback has decoded the result out of the buffer, which is what
  -- makes releasing the arena here safe: a guest that returned an array wrote it
  -- into arena memory, and `r` is already a Lua value by now.
  if outermost then leave_outermost() end
  if not ok then error(r, 0) end
  return r
end

-- Descriptor kinds, matching the guest-side constants in fk.
local REG_COMMAND, REG_INTERFACE = 1, 2

function register_callback(kind, descp)
  if not E.fk_on_call then return H.ERR_NO_MEMBER end
  local ok, desc = pcall(H.read_dyn, descp)
  if not ok or type(desc) ~= "table" then return H.ERR_BAD_ARGS end
  if kind == REG_COMMAND then
    -- {name = "...", help = <LocalisedString>, id = <u32>}
    if type(desc.name) ~= "string" or desc.name == "" or desc.id == nil then
      return H.ERR_BAD_ARGS
    end
    if commands == nil then return H.ERR_INVALID end
    local id = desc.id
    -- The help text is a LocalisedString, which is a string OR a nested array;
    -- read_dyn produces both shapes already and Factorio accepts either.
    local cok, cerr = pcall(commands.add_command, desc.name, desc.help or "",
      function(e) return invoke_callback(id, e) end)
    if not cok then
      log("fklua: commands.add_command(" .. desc.name .. ") failed: " ..
        tostring(cerr))
      return H.ERR_CALL_FAILED
    end
    return H.OK
  elseif kind == REG_INTERFACE then
    -- {name = "...", methods = {["method"] = <u32>, ...}}
    if type(desc.name) ~= "string" or desc.name == "" or
       type(desc.methods) ~= "table" then
      return H.ERR_BAD_ARGS
    end
    if remote == nil then return H.ERR_INVALID end
    local fns, n = {}, 0
    for mname, mid in pairs(desc.methods) do
      if type(mname) ~= "string" or type(mid) ~= "number" then
        return H.ERR_BAD_ARGS
      end
      local id = mid
      fns[mname] = function(...) return invoke_callback(id, ...) end
      n = n + 1
    end
    if n == 0 then return H.ERR_BAD_ARGS end
    local cok, cerr = pcall(remote.add_interface, desc.name, fns)
    if not cok then
      log("fklua: remote.add_interface(" .. desc.name .. ") failed: " ..
        tostring(cerr))
      return H.ERR_CALL_FAILED
    end
    return H.OK
  end
  return H.ERR_BAD_ARGS
end

-- The outbound half: remote.call with the arguments unpacked out of tier 2.
--
-- `unpack` is used here and is deliberately NOT used by fk_abi's M.call, which
-- dispatches on arity through four fixed shapes instead. The difference is not
-- inconsistency: M.call is on the hot path -- a 4x4 balancer recompile is ~350
-- of them in one tick -- and this is mod-to-mod interop, which happens at the
-- rate another mod's script decides to ask us something. Paying a table
-- construction and an unpack to get an arity nothing here can bound is the
-- right trade in one of those places and the wrong one in the other.
function remote_call(callp, retp)
  if remote == nil then return H.ERR_INVALID end
  local ok, req = pcall(H.read_dyn, callp)
  if not ok or type(req) ~= "table" then return H.ERR_BAD_ARGS end
  local iface, fname, args = req[1], req[2], req[3]
  if type(iface) ~= "string" or type(fname) ~= "string" then
    return H.ERR_BAD_ARGS
  end
  if args == nil then args = {} end
  if type(args) ~= "table" then return H.ERR_BAD_ARGS end
  local cok, r = pcall(remote.call, iface, fname, unpack(args, 1, #args))
  if not cok then
    -- A remote.call into an interface that is not there is an ordinary runtime
    -- condition -- the other mod may simply not be installed -- so it is a
    -- status rather than an error raised into the guest. The guest sees
    -- ERR_CALL_FAILED and decides.
    return H.ERR_CALL_FAILED
  end
  if retp ~= 0 then
    local st = H.write_dyn(retp, r)
    if st ~= H.OK then return st end
  end
  return H.OK
end

-- ---------------------------------------------------------------------------
-- Deferred work -- many events in one tick, ONE flush.
--
-- The downstream request was for an "end-of-dispatch" hook, on the grounds that
-- `dispatch` already knows when the outermost call returns. It does, and a hook
-- there would not have batched anything, because of what Factorio actually
-- delivers: a blueprint pasted as real entities raises one on_built_entity PER
-- ENTITY, each raised by the engine's own loop rather than from inside another
-- event. So `depth` goes 0 -> 1 -> 0 P times in one tick, every one of them an
-- outermost dispatch, and a dispatch_done hook fires P times. Nesting is the
-- other thing entirely -- create_entity{raise_built=true} raising from inside a
-- handler -- and it is not what a paste is.
--
-- What batches is a QUEUE THE GUEST OWNS plus a point in time to flush it, and
-- the only per-tick point Factorio offers is on_tick. Subscribing to on_tick
-- forever to notice that a burst ended is exactly the permanent cost the
-- request was trying to avoid, so this registers on_tick only while there is
-- something pending and tears it down again as soon as it has run:
--
--   guest calls fk.defer() any number of times during tick T
--     -> one on_tick dispatcher is registered, once
--   tick T+1
--     -> the handler unregisters itself, Factorio stops calling in
--     -> fk_on_deferred() runs once, and the guest drains its own queue
--
-- Steady-state cost is zero registrations and zero calls, which the tests
-- assert directly rather than leaving as a claim. The cost is a ONE TICK
-- LATENCY: there is no end-of-tick hook in this API, and on_tick for the
-- current tick has already been raised by the time a build event arrives, so
-- the earliest honest flush point is the next tick.
--
-- FACTORIO DOES NOT SAVE EVENT REGISTRATIONS. A mod re-registers everything
-- from on_load, so the armed flag lives in `storage` -- which is saved -- and
-- the load path below re-arms from it. Without that, a save taken between the
-- defer and the flush comes back with the guest's queue full and nothing
-- registered to drain it.
-- ---------------------------------------------------------------------------

local deferred_armed = false
local flush_deferred

-- Unregisters BEFORE dispatching, so a guest that defers again from inside its
-- own flush arms a fresh one-shot for the next tick rather than having it
-- removed by a teardown that has not happened yet.
flush_deferred = function()
  off_event(defines.events.on_tick, flush_deferred)
  deferred_armed = false
  if storage then storage.fk_deferred = nil end
  dispatch(E.fk_on_deferred)
end

-- ---------------------------------------------------------------------------
-- The first tick after a LOAD.
--
-- Factorio's own on_load cannot touch `game`: it runs on every client when
-- joining a multiplayer game and is read-only with respect to `storage`, so a
-- guest that wants to rebuild its state FROM THE WORLD -- which is what
-- --persist=none plus a world scan is -- has nothing to hang that off. Before
-- this the only way to notice a load was to subscribe to on_tick forever, a
-- permanent per-tick cost to observe a once-per-session event.
--
-- Same machinery as the deferred flush, and that is the point of having built
-- off_event: a one-shot on_tick registered from on_load and torn down by its own
-- handler. By the time it runs the game exists and every API call is legal.
--
-- It fires only after a LOAD, never on a new map -- script.on_load does not run
-- for one, and fk_on_init already covers that case. Nothing here is stored,
-- because on_load runs on every load by definition; there is no state to carry.
-- ---------------------------------------------------------------------------

local after_load_armed = false
local run_after_load

run_after_load = function()
  off_event(defines.events.on_tick, run_after_load)
  after_load_armed = false
  dispatch(E.fk_after_load)
end

local function arm_after_load()
  if not E.fk_after_load or after_load_armed then return end
  after_load_armed = true
  on_event(defines.events.on_tick, run_after_load)
end

-- Idempotent on purpose: a paste of ten thousand parts calls this ten thousand
-- times and registers one handler.
function arm_deferred()
  if not E.fk_on_deferred then return H.ERR_NO_MEMBER end
  -- `storage` does not exist while control.lua is loading, which is where a
  -- guest's package initialisers run. Arming from there still works for this
  -- session; there is simply no save yet for the flag to survive into.
  if storage then storage.fk_deferred = true end
  if deferred_armed then return H.OK end
  deferred_armed = true
  on_event(defines.events.on_tick, flush_deferred)
  return H.OK
end

-- ---------------------------------------------------------------------------
-- Instantiation and event wiring.
-- ---------------------------------------------------------------------------

-- TinyGo builds a reactor: _initialize sets up the heap and runs package
-- initialisers, and every //go:wasmexport function raises until it has.
--
-- It runs here rather than in on_init so that it also runs on load, which every
-- mode needs: the exports are unusable until it has, and under persistence the
-- memory it builds is immediately replaced by the saved one. Rebuilding and
-- then discarding is a few milliseconds at load and buys a guest that cannot
-- observe which of the two paths it came back on.
if E._initialize then E._initialize() end

-- The string scratch region, so a returned string costs a bump rather than a
-- call into the guest's allocator -- measured at ~53% of a real string return.
-- The GUEST owns the address for the same reason it owns fk_alloc's: a pointer
-- the host invented would land in the middle of something. Two exports rather
-- than one returning a pair, because multivalue is not in the feature set
-- FkLua compiles.
--
-- AFTER _initialize, not up with the other binds, and that placement cost an
-- afternoon. These are //go:wasmexport functions like any other: TinyGo's
-- runtime raises `unreachable` from every one of them until _initialize has
-- run, and the trap arrives with no name attached to it. The bind block above
-- deliberately runs first because it only STORES functions; this one CALLS
-- two, which is a different thing.
--
-- A guest without the exports, or one whose package initialisers make a host
-- call before this point, keeps the allocator path unchanged -- the region is
-- an optimisation and the fallback is always there.
if E.fk_scratch_base and E.fk_scratch_size then
  H.bind_scratch(E.fk_scratch_base(), E.fk_scratch_size())
end

-- ---------------------------------------------------------------------------
-- Persistence.
--
-- The mode is a property of the MODULE, not of this file: fklua stamps it into
-- the generated chunk at compile time, and control.lua ships as a complete
-- runtime that can drive any of them. That is what keeps this file copied out
-- verbatim instead of generated.
--
--   "none"   guest memory is rebuilt from the data segments on every load.
--            Deterministic, and nothing accumulated during play survives.
--   "table"  storage.fk_mem IS the word table the guest writes into, so a store
--            lands in the saved structure with no sync step at all.
--
-- MEMORY is free in table mode precisely because of that aliasing. GLOBALS are
-- not: a wasm global is a Lua local, and a local cannot alias a table field, so
-- they are copied after every guest call -- into a buffer allocated once, which
-- is one table write per mutable global per event. A TinyGo guest has one.
--
-- `storage` is unavailable at this (chunk) level, which is exactly why the
-- buffer is declared here and only ever assigned inside a handler.
-- ---------------------------------------------------------------------------

local P = instance.persist
local packed = P ~= nil and P.mode == "packed"
local persisting = P ~= nil and (P.mode == "table" or packed)
local gbuf
-- In packed mode this is the live page array, the same table that sits in
-- storage. Pages are replaced in place, so the reference stays valid.
local pages

-- Arming turns the store-side page marking on. It costs one upvalue read and one
-- test per store in the other modes, and is what makes flush incremental here.
if packed and P.arm then P.arm() end

-- WHAT A LARGE GUEST HEAP COSTS, AND WHY NOTHING ELSE CAN SEE IT.
--
-- The live linear memory is a vector of 2 MiB SHARDS, each a Lua array table
-- with one slot per 32-bit word, in EVERY persist mode -- `packed` changes what
-- enters `storage`, not what the guest runs on. Lua 5.2 traverses a table in a
-- single `propagatemark`: one gray object, one indivisible unit of work. What
-- sharding changes is WHICH table that is -- the worst single unit is now one
-- 2 MiB shard rather than the whole memory -- and what it does not change is
-- the total: the collector still walks every word every cycle, just in
-- 524,288-word pieces it may split across ticks.
-- `collectgarbage("setpause")` and `("setstepmul")` DO exist in Factorio's
-- sandbox and were measured against the flat shape; they moved a 12.7 ms pause
-- by less than its own run-to-run noise, because there was nothing to pace.
-- With shards there is.
--
-- 0.2 ms per MiB, flat from 8 MiB to 128, is the FLAT-TABLE measurement and it
-- is the number quoted below. Sharding changed the table count, so it is due a
-- re-measurement; see agents/guests.md, "the guest heap budget".
--
-- It is memory SIZE, not memory used. `mem_grow` writes a zero into every new
-- word, and TinyGo's wasm allocator DOUBLES the memory each time it runs out, so
-- a guest that needs 65 MiB gets 128 and pays ~26 ms of worst tick for a heap
-- that is half untouched. This line is the only place that number is visible:
-- the first downstream mod spent two milestones attributing the pause to
-- `--persist` -- the one thing it is not -- with no way to see its own heap.
--
-- Once per doubling from 16 MiB up, which is where the pause stops being
-- invisible (~3 ms) and where TinyGo's ladder puts it anyway.
local memnoted = 0
local function note_memory(size)
  if size < 16777216 or size < memnoted * 2 then return end
  memnoted = size
  local mib = size / 1048576
  -- WHAT THIS LINE SAYS CHANGED WHEN MEMORY BECAME SHARDED, and saying the old
  -- thing would now be wrong in the direction that frightens people for no
  -- reason. It used to report `mib * 0.2` as a WORST TICK, because the memory
  -- was one Lua table and `propagatemark` walks one table in a single step it
  -- cannot split. It is a vector of 2 MiB shards now, so the total work is the
  -- same and the worst tick is bounded by ONE shard -- measured in game at
  -- 0.417-0.426 ms across heaps of 2.7 and 6.7 MiB, i.e. flat in memory size.
  -- Both halves are reported because a mod author needs both: the total is
  -- what they pay per cycle, the tick is what a player feels.
  log(string.format("fklua: this guest's linear memory is now %d MiB. Lua's " ..
    "collector walks every word of it each cycle -- about %.1f ms of total " ..
    "work -- but in %d shards of 2 MiB, so no single tick carries more than " ..
    "about 0.4 ms of it. It is the SIZE of the memory, not the part in use, " ..
    "and no --persist mode changes it. See agents/guests.md, \"the guest heap " ..
    "budget\".", mib, mib * 0.2, math.ceil(size / 2097152)))
end

-- THE 4 MiB WALL IS GONE, AND SO IS THE NOTICE THAT REPORTED IT.
--
-- The word table used to be ONE table, so 4 MiB of linear memory was 2^20 =
-- 1,048,576 keys -- and in Factorio's Lua that is a cliff, not a slope. A guest
-- that crossed it paid a ~2.7-SECOND tick once, then ~20x on every load and
-- store for the rest of the session, and ~2.9 s again on every LOAD while this
-- file rebuilt the table. `note_wall` existed to say so, because it was
-- reportable and not fixable from Lua.
--
-- IT IS FIXABLE FROM LUA. Linear memory is now a vector of 2^19-word shards, so
-- no table the guest runs on can ever hold more than half the wall's keys, at
-- any memory size. Every sentence the notice printed is now false -- there is no
-- crossing tick, no 20x, and a load is ~26 ms/MiB flat with no cliff in it --
-- and a false notice is worse than none. It is deleted rather than repointed:
-- the residual cost that DOES still scale with memory size is the collector's
-- walk, and note_memory above already reports exactly that.
--
-- This also closes the defect agents/sharding.md section 10 left for triage --
-- note_wall was reached from the chunk-level P.memory(), which sees the
-- DECLARED size rather than the adopted one, and from sync_memory, which only
-- fires when the size CHANGES, so a guest that crossed in a previous session
-- came back past the wall and said nothing. Deleting the notice closes it in
-- the only way that stays true.

-- The initial declared memory, in every mode including `none`. A GROW is only
-- seen in the persisting modes, where sync_memory already compares the size for
-- its own reasons so the notice costs nothing; a `--persist=none` guest that
-- grows is the one case this cannot report.
if P ~= nil and P.memory then
  local _, size0 = P.memory()
  note_memory(size0)
end

-- ---------------------------------------------------------------------------
-- PACING THE MEMORY PRE-BUILD -- one bounded piece per tick, and only while a
-- growing guest is owed one.
--
-- `mem_grow` writes a zero into every new word at about 107 ns a word in
-- Factorio's Lua, and there is no fixed cost to trade that against: reaching
-- 40 MiB in 640 one-page grows costs 0.984x what reaching it in 10 four-megabyte
-- ones costs (scripts/run-growprobe.sh). So the work cannot be avoided and the
-- only question is which tick it lands on -- and left alone the answer was the
-- growing tick, 22.7-30.0 ms at a 3.5 MiB heap and 288-365 ms at 40 MiB. Since
-- sharding stage C paced the collector, that was the worst tick a growing guest
-- had, by two orders of magnitude over the collector's own worst step.
--
-- The runtime keeps a fill cursor ahead of MEMSIZE; this is what advances it.
-- A grow that lands inside pre-built words costs 1.2-2.7 us instead of
-- milliseconds, and pacing the pre-build costs 2-3% over doing it in one go.
--
-- IT IS THE fk.defer / fk_gc_step SHAPE, for the third time and for the same
-- reason: a guest that is not growing must carry no per-tick handler. The
-- runtime calls `arm_prebuild` from the arming path of a real memory.grow and
-- from nowhere else, so a guest whose declared memory is enough -- which is
-- most of them -- never registers anything, and a guest that grew once and
-- stopped unregisters as soon as its lookahead is full.
--
-- IT IS NOT GATED ON --persist OR ON --gc. Both growth laws that reach
-- mem_grow are outside this file: fkgc grows by a quarter and TinyGo's own
-- growHeap DOUBLES, and a `--persist=none` guest under `-gc=leaking` pays the
-- same 107 ns a word as a collected one in table mode.
--
-- PREBUILD_BUDGET is words per tick, and 8,192 is about 0.9 ms in game -- the
-- same order as the collector's own 1,024-granule step, deliberately, because
-- a guest that is both growing and collecting should not spend two frames'
-- worth of one tick on housekeeping. It is the piece size the probe measured
-- the 1.02-1.03x pacing overhead at.
local PREBUILD_BUDGET = 8192
local prebuild_armed = false
local prebuild_step

-- Unregisters BEFORE doing the work, so a grow that happens during this tick
-- (it cannot today -- nothing here calls the guest -- but the collector's step
-- has the same note for the same reason) gets a fresh one-shot rather than
-- having it torn down by a teardown that has not happened yet.
prebuild_step = function()
  off_event(defines.events.on_tick, prebuild_step)
  prebuild_armed = false
  if P.prebuild(PREBUILD_BUDGET) then
    prebuild_armed = true
    on_event(defines.events.on_tick, prebuild_step)
  end
end

-- Idempotent, like arm_deferred and arm_gc: a guest that grows on every tick of
-- a long climb registers one handler.
--
-- Nothing about this is saved. The cursor is reset to the loaded size by
-- `adopt`/`restore`, so a freshly loaded guest is owed nothing and there is no
-- registration to re-establish from on_load -- unlike the collector, whose
-- in-flight mark has to survive the save (`storage.fk_gc`).
if P ~= nil and P.grow_hook and P.prebuild then
  P.grow_hook(function()
    if prebuild_armed then return end
    prebuild_armed = true
    on_event(defines.events.on_tick, prebuild_step)
  end)
end

-- Called after every guest entry point, and only then: a global cannot change
-- while no guest code is running.
local function sync_globals()
  if gbuf then P.globals(gbuf) end
end

-- Called after every guest entry point, and only then.
--
-- In table mode a guest's STORES need nothing done: storage.fk_mem is the live
-- table, which is the whole reason table mode is the default. A GROW is the
-- exception and it took a wasip1 guest to find it -- memory.grow replaces the
-- table and moves the size, and storage.fk_memsize was written once at
-- on_init and never again.
--
-- The symptom was as far from the cause as it gets: the guest ran correctly
-- for as long as the session lasted, the save recorded the OLD size, and the
-- next load adopted a memory whose bounds stopped short of the heap the guest
-- had already grown into. Every access past the old boundary trapped, inside
-- guest code, on a tick that had worked a thousand times before the save.
--
-- Nothing before wasip1 grew: -gc=leaking never returns memory and never asked
-- for more than its initial pages.
-- PACKED MODE HAS THE SAME GROW PROBLEM AND IT WAS OUTSIDE THE M10 FIX.
-- The size mirror was written once at state_init and never again, so a packed
-- guest that grew saved its new pages against its OLD size -- and state_load
-- hands that size to restore, which is what decides both MEMSIZE and how much
-- of the save is read back. Same symptom as above, one mode over: a heap that
-- worked all session comes back short.
local function sync_memory()
  if not persisting then return end
  if packed then
    if pages then P.flush(pages) end
    if P.memory then
      local _, size = P.memory()
      if storage.fk_memsize ~= size then
        storage.fk_memsize = size
        note_memory(size)
      end
    end
    return
  end
  if P.memory then
    local mem, size = P.memory()
    if storage.fk_memsize ~= size then
      -- THE REASSIGNMENT IS NOW REDUNDANT AND IS KEPT ANYWAY. Under sharding a
      -- grow APPENDS a shard to a table `storage` already holds, so the alias
      -- survives a grow where the flat form replaced the whole table and had
      -- to be re-pointed. Re-pointing it to the same object costs one table
      -- store on the rare path that already noticed a size change, and it is
      -- what keeps this correct if anything ever DOES replace the vector.
      -- storage.fk_memsize still needs its refresh either way -- that half was
      -- never about the table.
      storage.fk_mem = mem
      storage.fk_memsize = size
      note_memory(size)
    end
  end
end

-- Everything that has to happen when a dispatch ends, however it ended.
--
-- Releasing the transient handles is the half that matters: a handler that
-- reads event.entity and drops it is the dominant shape in real mod code, and
-- under a single handle space every one of those would pin an entry forever.
-- Dropping the whole space here means the default leaks nothing.
function dispatch_done()
  H.clear_transient()
  sync_memory()
  sync_globals()
end

-- ---------------------------------------------------------------------------
-- PACING THE COLLECTOR -- one bounded step per tick, and only while collecting.
--
-- A guest built --gc=collected imports `fk.gc` and calls it from
-- fkgc.CollectIfNeeded when heap pressure says a collection is due. That is the
-- only entry point: nothing here polls, nothing here subscribes to on_tick
-- permanently, and a guest with a small heap that never fills registers nothing
-- and pays nothing. It is exactly the fk.defer machinery with a different
-- payload, which is what agents/gc.md section 4 predicted before either existed.
--
--   guest calls fk.gc() during tick T
--     -> the write barrier is armed, one on_tick dispatcher is registered
--   tick T+1, T+2, ...
--     -> the handler unregisters itself, drains the dirty page set into the
--        guest's buffer, runs ONE bounded fk_gc_step, and re-registers if the
--        collection has not finished
--   the step that finishes it
--     -> disarms the barrier, does not re-register, clears the storage flag
--
-- THE BARRIER IS ARMED FOR THE MARK PHASE ONLY. fk_gc_step returns the phase it
-- left the collector in -- 0 idle, 1 marking, 2 sweeping -- and the sweep runs
-- with the barrier off, because the mark bitmap is fixed once marking terminates
-- and no store can change a decision the sweep makes. That halves the window in
-- which a guest pays the 7-13% armed store cost agents/gc.md measured, and it is
-- the reason the expensive phase is the cheap one to incrementalize.
--
-- THE SAFE POINT. A step may run only where the guest holds no live reference
-- anywhere but the heap and its statics -- which on this target means an
-- OUTERMOST dispatch boundary, where the wasm operand stack and the shadow
-- stack are both empty. on_tick is raised by the engine's own loop and never
-- from inside another event, so a step reached from here is outermost by
-- construction. That is asserted rather than assumed, because it is the
-- precondition the whole marking argument rests on and the assertion costs one
-- compare per collection step.
--
-- FACTORIO DOES NOT SAVE EVENT REGISTRATIONS, and it does not save the page set
-- either -- the set is a Lua table inside the generated chunk, not a `storage`
-- entry. So `storage.fk_gc` carries "a collection was in progress" across a
-- save the way `storage.fk_deferred` carries a pending flush, and the first step
-- after a load is told the page record was LOST. The collector's answer to that
-- is one budgeted, resumable re-scan of everything it had marked, which is the
-- same recovery it already uses for gray-stack overflow.
-- ---------------------------------------------------------------------------

local GC = P ~= nil and P.gc or nil
local gc_base, gc_cap = 0, 0
local gc_armed = false
local gc_lost = false
local gc_step

-- The dirty-page buffer is the GUEST's, for the same reason fk_scratch_base is:
-- an address the host invented would land in the middle of something. It is
-- inside the collector's own .bss struct, which is what keeps the page numbers
-- in it from being read back as candidate pointers by the next root scan.
--
-- Read AFTER _initialize, like bind_scratch above and for the same reason: these
-- are //go:wasmexport functions and TinyGo's runtime raises `unreachable` from
-- every one of them until the module has initialised.
if GC and E.fk_gc_step and E.fk_gc_dirty_base and E.fk_gc_dirty_cap then
  gc_base = E.fk_gc_dirty_base()
  gc_cap = E.fk_gc_dirty_cap()
  if gc_cap == 0 then GC = nil end
else
  GC = nil
end

-- 4294967295 is fkgc.DirtyAll: "the record of what changed was lost, assume
-- everything did". Spelled here rather than imported because the guest side is
-- Go and this side is a hand-written Lua file that ships verbatim.
local GC_DIRTY_ALL = 4294967295

-- Unregisters BEFORE dispatching, so a guest that arms again from inside the
-- step's own dispatch gets a fresh one-shot rather than having it torn down by
-- a teardown that has not happened yet. Same shape as flush_deferred, same
-- reason.
gc_step = function()
  off_event(defines.events.on_tick, gc_step)
  gc_armed = false
  if dispatch_depth() ~= 0 then
    -- Unreachable today: on_tick is raised by the engine's own loop and never
    -- from inside another event. It is asserted anyway because everything the
    -- marking argument claims -- that a deleted reference cannot be hiding in a
    -- register or a stack slot, so the roots plus the dirty pages are the whole
    -- of what has to be re-scanned -- is false at a nested dispatch, and the
    -- failure would be a live object swept rather than an error.
    error("fklua: a collection step was reached at dispatch depth " ..
          tostring(dispatch_depth()) .. ". A step may run only at an OUTERMOST " ..
          "dispatch, where the guest's shadow stack is empty; anywhere else " ..
          "the conservative scan cannot see a reference the mutator is still " ..
          "holding. See agents/gc.md, the safe-point precondition.", 0)
  end
  local n
  if gc_lost then
    gc_lost = false
    n = GC_DIRTY_ALL
  else
    n = GC.drain(gc_base, gc_cap)
  end
  local phase = dispatch(E.fk_gc_step, n)
  if phase == 1 then GC.arm() else GC.disarm() end
  if phase ~= 0 then
    if not gc_armed then
      gc_armed = true
      on_event(defines.events.on_tick, gc_step)
    end
  elseif storage then
    storage.fk_gc = nil
  end
end

-- Idempotent, like arm_deferred: a guest that calls this on every tick of a
-- long collection registers one handler.
function arm_gc()
  if not GC then return H.ERR_NO_MEMBER end
  -- Armed here rather than after the first step, because the guest calls this
  -- from inside its own handler and keeps running afterwards. Every store it
  -- makes between this line and the first step has to be recorded.
  GC.arm()
  -- `storage` does not exist while control.lua is loading, which is where a
  -- guest's package initialisers run. Arming from there still works for this
  -- session; there is simply no save yet for the flag to survive into.
  if storage then storage.fk_gc = true end
  if gc_armed then return H.OK end
  gc_armed = true
  on_event(defines.events.on_tick, gc_step)
  return H.OK
end

-- Alias the per-level static buffer caches into `storage`, so that a load finds
-- the buffers this heap already contains instead of allocating a second set.
--
-- Defined down here rather than beside the two caches because it reads
-- `persisting`, which is a property of the compiled module and is not known
-- until the persistence block above; forward-declared up there because it has
-- to be CALLED at the allocation. See event_buffer's header for why that is the
-- only legal place for the write and why state_init alone would not be enough.
--
-- Idempotent, and after state_init or state_load the two assignments are
-- already true -- the tables ARE the ones in `storage` by then. What it does on
-- its own is establish the mirror the first time, over a save that predates it.
--
-- Under --persist=none there is nothing to mirror: memory is rebuilt from the
-- data segments on every load, so a pointer from the previous session names a
-- byte the guest never wrote, and a fresh cache is the CORRECT answer rather
-- than a missed optimisation. Such a mod also touches `storage` nowhere else,
-- which is a property this must not be the one thing to break.
--
-- THE SIZE TRAVELS WITH THE ADDRESSES, and that is the one thing here that is
-- not just bookkeeping. An address says where a buffer starts and nothing about
-- how much of the heap belongs to it, and the size these were allocated at is
-- not a constant of the guest -- API.event_scratch is the largest subscribed
-- event's payload, which comes out of the PACKAGED event table. Two packages of
-- the same wasm against two API pins can disagree about it, and fk_migrate_adopt
-- hands over another build's heap outright. (The pin is folded into the build
-- stamp since 2026-08-07, so the first of those two no longer reaches here
-- through same_build(); fk_migrate_adopt still does, deliberately, which is why
-- this stays.) Reusing a buffer that was allocated smaller
-- than what write_struct is about to put in it is a silent overwrite of
-- whatever the guest allocated next, so state_load refuses a cache whose
-- recorded size is not the size being asked for and lets the allocation happen
-- again. One buffer, once, against a class of corruption with no error message.
function publish_buffers()
  if not (persisting and storage) then return end
  local b = storage.fk_bufs
  if b == nil then
    b = {}
    storage.fk_bufs = b
  end
  b.ev, b.call = scratch, callbuf
  b.evn, b.calln = API.event_scratch, H.DYNW * 2
end

local function state_init()
  if not persisting then return end
  -- WHATEVER WAS OUTSTANDING IS SETTLED BY GETTING HERE, and clearing it here
  -- rather than only in finish_rebuild is what stops the handling firing twice.
  -- Factorio raises on_init for a mod ADDED to an existing save, and it raises
  -- on_load for that same load in some orderings -- so state_load can set the
  -- flag over a `storage` that on_init is about to fill in properly. This line
  -- is the one place both paths pass through, and after it the stamp below is
  -- current by construction, so there is nothing left to tell anybody about.
  rebuild_pending = false
  if packed then
    if P.pack then
      pages = P.pack()
      storage.fk_pages = pages
      local _, size = P.memory()
      storage.fk_memsize = size
    end
  elseif P.memory then
    local mem, size = P.memory()
    storage.fk_mem = mem
    storage.fk_memsize = size
  end
  if P.globals then
    gbuf = P.globals()
    storage.fk_globals = gbuf
  end
  -- THE PERSISTENT HANDLE SPACE, published exactly like the heap.
  --
  -- fk_abi.lua's header has always said a retained handle "lives in `storage`
  -- so it survives a save", and both halves of the mechanism were there --
  -- persistent_table() hands over the live table, adopt() takes it back and
  -- rebuilds the free list. This file called neither, so the space was
  -- session-scoped and the promise had nothing behind it. Downstream (F1) found
  -- it the way it has to be found: the guest's own state came back intact and
  -- every handle inside it resolved to ERR_BAD_HANDLE.
  --
  -- It is the LIVE table, aliased into storage the way --persist=table aliases
  -- the word table, so a retain during play lands in what Factorio serializes
  -- with no sync step -- which is also why it is published unconditionally
  -- rather than inside the packed/table branch above: what differs between
  -- those modes is how MEMORY is mirrored, and this is not memory.
  --
  -- Written on every state_init, including the rebuilt-guest one below, so a
  -- discarded heap discards its handles with it. See state_load.
  storage.fk_handles = H.persistent_table()
  -- ...AND THE STATIC BUFFER CACHES, published the same way and for the same
  -- reason as the handle space above: this runs where the heap is FRESH, so the
  -- mirror has to be reset to the fresh (empty) caches with it. Dropping the
  -- whole entry first rather than overwriting its two fields, so a save that
  -- carried a key a later runtime stopped writing does not keep it forever.
  --
  -- On the rebuilt-guest path this is reached from on_configuration_changed,
  -- after the migrate dispatch and after state_load declined to adopt -- so the
  -- caches really are the empty ones this session's require built, and the
  -- previous build's pointers go out of `storage` with the heap they described.
  storage.fk_bufs = nil
  publish_buffers()
  storage.fk_build = P.build
  storage.fk_state = E.fk_state_version and E.fk_state_version() or 0
end

-- Whether the heap in this save was written by the build that is now running.
--
-- Any change to the guest moves its layout: static addresses shift, struct
-- offsets move, the allocator starts somewhere else. A heap written by one
-- build and read by another is not stale data, it is undefined -- so this is
-- the difference between losing a guest's state and corrupting it.
--
-- A BUILD IS THE MODULE AND THE API PIN IT WAS PACKAGED AGAINST, which is a
-- fact about the stamp rather than about this comparison -- nothing here
-- changed when the pin was folded in. It matters at this line because the pin
-- decides the packaged member, event and define tables, whose ids are dense
-- indices over one version's set, and because API.event_scratch below is read
-- out of the packaged event table and is a size a buffer IN THIS HEAP was
-- allocated at. See cmd/fklua's buildID.
--
-- A save with no stamp (written before persistence existed, or by
-- --persist=none) is treated as a different build, which is the safe answer.
local function same_build()
  return storage.fk_build ~= nil and storage.fk_build == P.build
end

-- on_load is READ-ONLY with respect to storage, and has to be: Factorio runs it
-- on every client when joining a multiplayer game, and a write here is a desync
-- waiting to happen. Adopting a table is a read of storage and a write to the
-- guest's own upvalues, which is not the same thing.
--
-- Factorio runs on_load BEFORE on_configuration_changed, so the decision to
-- adopt has to be made without knowing whether a migration is about to happen.
-- It is made on the build stamp alone, which is available here and is the same
-- on every client -- so every client makes it identically, which is what keeps
-- a multiplayer join deterministic.
--
-- A guest that exports fk_migrate is asking to be handed the old heap so it can
-- fix it up. One that does not gets the freshly-initialised heap that
-- _initialize already built, and never sees the old bytes at all.
-- ADOPTION IS A SEPARATE EXPORT FROM MIGRATION, and it did not used to be.
--
-- fk_migrate alone used to mean "hand me the old build's memory and I will fix
-- it up in place", and a guest could not do that -- because linear memory is not
-- just the heap. It is .data and .rodata too, and a rebuilt guest refers to its
-- string constants, its type descriptors and its static buffers by COMPILED-IN
-- ADDRESS. Every one of those addresses now points at whatever the previous
-- build put there, so the very first thing fk_migrate does is already undefined:
-- it sends the host a string it reads out of somebody else's rodata. The offer
-- was a choice between losing state silently and corrupting it silently, and the
-- first downstream mod took the loss and rebuilt from the world instead.
--
-- So the two are split, and the SAFE one is the one with the obvious name:
--
--   fk_migrate(old_version)        the heap is FRESH -- exactly what
--                                  _initialize just built. The guest is simply
--                                  TOLD that this save was written by another
--                                  build, which is all it needs to rebuild from
--                                  the world. Nothing is undefined.
--   fk_migrate_adopt(old_version)  the old heap IS adopted, rodata and all.
--                                  Opt-in, for a guest whose state is a fixed
--                                  versioned region it interprets itself --
--                                  hand-written wasm, a #[repr(C)] blob -- and
--                                  which can therefore survive reading its own
--                                  constants from the wrong build. A Go or Rust
--                                  guest is not that and should not export it.
--
-- WHAT THIS FUNCTION CANNOT DO IS FINISH THE JOB, and for two milestones it did
-- not have to, because the only rebuild anyone tested was one that also changed
-- the mod's VERSION. A build stamp moving with the mod's version left unchanged
-- is what every dev rebuild is, and Factorio does not raise
-- on_configuration_changed for one -- so the branch below returned, nothing
-- republished the stamp, and the save stayed permanently self-inconsistent. See
-- finish_rebuild.
local function state_load()
  if not persisting then return end
  local matched = same_build()
  -- RECORDED BEFORE THE GATE, because BOTH arms of a stamp mismatch have
  -- unfinished business and only one of them is the discard. A guest exporting
  -- fk_migrate_adopt falls through the gate and really does adopt -- and is then
  -- owed the fk_migrate_adopt(old_version) call telling it whose bytes it is
  -- running on, plus a republished stamp, exactly as the discarding arm is owed
  -- fk_migrate and a fresh one. Gating this on the discard would have fixed the
  -- louder half and left the adopt arm silently never notified, for ever, which
  -- is the shape this repo keeps calling "a guard written for the first instance
  -- of a pattern".
  --
  -- A WRITE TO AN UPVALUE, WHICH IS THE ONE THING on_load MAY DO -- the same
  -- distinction the header above draws for adopting the word table, and the
  -- reason the ACT this flag stands for happens somewhere else entirely.
  if not matched then rebuild_pending = true end
  if not (matched or E.fk_migrate_adopt) then return end
  if packed then
    -- The live word table is the one _initialize just built; the saved pages
    -- are unpacked ON TOP of it. Nothing from storage stays referenced, which
    -- is the difference from table mode: here storage holds a mirror, there it
    -- holds the thing itself.
    if P.restore and storage.fk_pages then
      P.restore(storage.fk_pages, storage.fk_memsize)
      pages = storage.fk_pages
    end
  elseif P.adopt and storage.fk_mem then
    P.adopt(storage.fk_mem, storage.fk_memsize)
  end
  if P.setglobals and storage.fk_globals then
    gbuf = storage.fk_globals
    P.setglobals(gbuf)
  end
  -- ...AND THE HANDLES COME BACK WITH THE HEAP, never on their own.
  --
  -- A handle is a NUMBER the guest wrote into its own memory. Adopting the
  -- table without the heap that remembers those numbers would pin the previous
  -- build's LuaObjects in `storage` forever, reachable by nobody -- a leak that
  -- grows with every rebuild and that no guest can free, because freeing needs
  -- the number. So this sits under the same `same_build() or fk_migrate_adopt`
  -- gate as everything above it, and the discard path needs no code at all:
  -- on_configuration_changed calls state_init, which publishes the live table
  -- this session started with, which is empty.
  --
  -- adopt() REBUILDS THE FREE LIST rather than restoring one, which is what
  -- keeps handle numbering deterministic across the load: every client adopts
  -- the same saved table and derives the same list from it, ascending. A stale
  -- free list read back from a save would hand out a slot still in use.
  --
  -- What a retained handle MEANS after a load is a separate question from
  -- whether it resolves, and both answers are useful: Factorio serializes the
  -- reference, so the handle resolves, and if the thing behind it was destroyed
  -- meanwhile the object's `valid` is false and the call is ERR_INVALID. That
  -- is the distinction ERR_BAD_HANDLE exists to make and the one a guest was
  -- being denied. See agents/abi.md, "What a retained handle means after a
  -- load".
  if storage.fk_handles then H.adopt(storage.fk_handles) end
  -- ...AND SO DO THE STATIC BUFFERS, for the same reason and under the same
  -- gate. A buffer address is a number into a heap laid out by one build, so
  -- reusing it is sound exactly where adopting that heap is sound and nowhere
  -- else -- which is what puts this here, below the gate, rather than at the
  -- top of the function. Under fk_migrate_adopt the old bytes really are what
  -- the guest is running on, so the old ADDRESSES are the right ones and the
  -- size test below is what covers the rest of that case; a guest that gets a
  -- FRESH heap never reaches this line and starts from an empty cache, which is
  -- what state_init republishes.
  --
  -- THIS IS A READ OF `storage` AND A WRITE TO TWO UPVALUES, which is the one
  -- thing on_load is allowed to do -- the same distinction the header above
  -- draws for adopting the word table and the handle space. Nothing here writes
  -- `storage`, and nothing may: this function runs on a joining client and on
  -- no other peer.
  --
  -- A save with no mirror in it was written by a runtime that had none. It
  -- leaves the caches empty, which is precisely the behaviour that save already
  -- had; the first allocation after it publishes one, so it is the last load
  -- that pays. A mirror recorded at a size this build does not ask for is
  -- refused the same way and for the reason publish_buffers gives -- an address
  -- does not carry its own length, and reusing one that is too small is a
  -- silent overwrite rather than a wasted allocation.
  local b = storage.fk_bufs
  if b then
    if b.ev and b.evn == API.event_scratch then scratch = b.ev end
    if b.call and b.calln == H.DYNW * 2 then callbuf = b.call end
  end
end

-- EVERYTHING A STAMP MISMATCH OWES, IN ONE FUNCTION WITH TWO CALLERS.
--
-- What it owes is three things: the guest is TOLD (fk_migrate on the fresh heap,
-- fk_migrate_adopt on the adopted one) or the loss is logged; and the `storage`
-- mirror is republished, stamp included, so the save this session writes is the
-- one THIS build wrote rather than a heap from one build carrying the stamp of
-- another.
--
-- IT USED TO LIVE INSIDE on_configuration_changed AND THAT WAS THE DEFECT.
-- Factorio raises on_configuration_changed when the mod's VERSION changes -- and
-- a build stamp moves for a great deal less than that. Every dev rebuild keeps
-- the version. So did the commit that folded the --api pin into the stamp, which
-- moved every stamp in existence at once. On all of those the hook never fired,
-- state_load's decline was never finished, and what was left behind was not a
-- reset but something considerably worse:
--
--   * the save is PERMANENTLY SELF-INCONSISTENT. Nothing republished
--     storage.fk_mem, so `storage` still holds the previous build's heap while
--     the guest runs on the fresh one _initialize built -- two unrelated tables,
--     and the guest's own writes reach neither the save nor the CRC.
--   * every later load declines again, because the stamp it compares against is
--     still the one that did not match.
--   * fk_migrate / fk_migrate_adopt NEVER FIRE. A guest whose entire answer to a
--     rebuild is "tell me and I will rescan the world" is never told.
--
-- AND ON A MULTIPLAYER JOIN IT IS A DESYNC, which is how it was found (measured
-- 2026-08-07). The server loads a stale-stamp save, declines, and runs on
-- happily from tick 0 for twenty minutes. A client joins, downloads that state,
-- reads the same stale stamp, declines identically -- and starts a TICK-0 heap
-- against a server whose guest is at tick 1250. Neither peer logs anything,
-- because the line that would have said so lives in the hook that did not fire.
-- The asymmetry is what made it read as a guest defect: only the join can see
-- it, and what it looks like is `crc test failed` from the first joined tick.
--
-- SO THE DECISION IS TAKEN AT LOAD AND THE ACT IS DEFERRED TO THE FIRST
-- REPLICATED EXECUTION POINT, which is the first OUTERMOST DISPATCH after the
-- load -- see enter_outermost. The determinism argument, which is the whole
-- reason this is legal:
--
--   * the trigger is a function of the LOADED STATE ALONE -- storage.fk_build
--     against P.build -- so every peer that loaded the same bytes computes the
--     same answer, with no clock, no entropy and no peer-local signal in it.
--     That is what an fk_after_load one-shot is NOT: it is armed on whichever
--     peers happen to load, which on a running server is the joining client and
--     nobody else, and a write from there is CLAUDE.md's named desync.
--   * a peer that JOINS LATER cannot disagree, because it downloads state that
--     has already been republished: the handling runs before any guest code the
--     load could reach, so the server's very first dispatch settles it, and from
--     then on same_build() is true and there is nothing to decline.
--   * the residual window is a peer joining a server that has declined and has
--     not yet dispatched ANYTHING. Then both peers hold the same flag over the
--     same stale state and both settle it at their next dispatch -- which is the
--     same dispatch, because every remaining source of one is replicated (a
--     tick, an event, the deferred flush, a collector step) and any peer-local
--     one (fk_after_load) is itself a dispatch, so a server that had reached it
--     would already have republished.
--   * on_configuration_changed keeps its call, as the EARLIER of the two
--     opportunities: it runs before the first tick, only on the peer that
--     loaded, and finishing there means a joiner cannot ever see the flag. It is
--     the same function, so the two paths cannot drift -- which they had, the
--     hook's own comment having claimed for two milestones that fk_migrate
--     adopts.
--
-- WHAT IT DOES NOT COVER, said rather than implied: a guest that never
-- dispatches at all never reaches here. Such a guest also never writes a word of
-- its own memory, so there is nothing for two peers to disagree about and
-- nothing for a save to lose -- the stale save simply round-trips unchanged, as
-- it did before. `fklua mod` already calls that guest Inert.
--
-- The flag is cleared BEFORE the dispatch, which is what makes it safe to reach
-- here from inside enter_outermost: dispatch() re-enters enter_outermost, finds
-- the flag down, and proceeds normally. A migrate that TRAPS takes the mod down
-- exactly as it does from on_configuration_changed today, and the next load
-- re-arms the flag from the stamp, which is still stale.
function finish_rebuild()
  if not rebuild_pending then return end
  -- Tested BEFORE the flag comes down, so a caller that somehow arrives without
  -- a `storage` to publish into leaves the work outstanding rather than
  -- swallowing it. The clear still happens before the dispatch below, which is
  -- the half that matters -- that is the recursion guard.
  if not (persisting and storage) then return end
  rebuild_pending = false
  local was = storage.fk_state or 0
  local migrate = E.fk_migrate_adopt or E.fk_migrate
  if migrate then
    dispatch(migrate, was)
  else
    log("fklua: this mod was rebuilt, so the guest heap saved by build " ..
        tostring(storage.fk_build) .. " cannot be read by build " ..
        tostring(P.build) .. ". Guest state has been reset. Export " ..
        "fk_migrate(old_version) to be told when that happens, and " ..
        "fk_migrate_adopt(old_version) if you also want the old bytes.")
  end
  state_init()
end

-- ONE script.on_load, because script.on_load REPLACES exactly the way
-- script.on_event does -- two callers here would silently leave only the last
-- one registered, which is the bug the per-event dispatcher list exists to
-- prevent one level down.
--
-- Registered only when something actually needs it. A mod compiled
-- --persist=none whose guest never defers touches neither on_load nor
-- `storage`, which keeps its behaviour exactly what it was before persistence
-- existed.
local function after_load()
  if persisting then state_load() end
  -- Event registrations do not survive a save; `storage` does. Re-arm from the
  -- flag so work deferred before the save runs on the first tick after the
  -- load, rather than sitting in the guest's queue forever.
  if storage and storage.fk_deferred and E.fk_on_deferred and not deferred_armed then
    deferred_armed = true
    on_event(defines.events.on_tick, flush_deferred)
  end
  -- A COLLECTION CAN NOW BE HALF DONE WHEN A SAVE IS TAKEN, which nothing before
  -- stage C could be. The collector's own state -- phase, mark bitmap, gray
  -- stack, sweep cursor, free runs -- is all in linear memory and came back with
  -- it, so resuming is a matter of scheduling steps again.
  --
  -- Two things did NOT come back and both are handled here rather than saved.
  -- The event registration, because Factorio does not save one. And the DIRTY
  -- PAGE SET, because it is a Lua table inside the generated chunk and no
  -- `storage` entry mirrors it -- so every write the mutator made between the
  -- last step and the save is unrecorded. The first step after a load is
  -- therefore told the record was lost, and re-scans everything it had marked.
  --
  -- The barrier is re-armed here rather than left to the first step's return
  -- value. It may be a tick of armed stores for a collection that turns out to
  -- have been sweeping, which needs no barrier; that is a few percent for one
  -- tick, against an argument about what may run between on_load and the first
  -- on_tick that nobody should have to make.
  if storage and storage.fk_gc and GC and not gc_armed then
    gc_lost = true
    gc_armed = true
    GC.arm()
    on_event(defines.events.on_tick, gc_step)
  end
  arm_after_load()
end

if persisting or E.fk_on_deferred or E.fk_after_load or GC then
  script.on_load(after_load)
end

-- on_configuration_changed is the EARLIER of the two places a rebuilt guest gets
-- dealt with, and for two milestones it read as the only one. It fires after
-- on_load and before the first tick, so finishing here puts everything in place
-- before any guest code runs -- which is strictly better than the first-dispatch
-- path in finish_rebuild, and is why this call is kept rather than folded away.
--
-- WHAT IT DOES NOT FIRE FOR IS THE POINT. Factorio raises this when the mod's
-- VERSION changes, and a build stamp moves for a dev rebuild, a --gc or --persist
-- change, or a repackage against another --api pin -- none of which touch the
-- version. The three outcomes below are state_load's, not this hook's; this hook
-- is one opportunity to reach them and the first outermost dispatch is the other.
--
--   same build          nothing to do; the heap was already adopted, and
--                       finish_rebuild returns on the flag state_load did not
--                       set.
--   changed, fk_migrate_adopt
--                       the old heap WAS adopted -- state_load's gate lets it
--                       through -- and the guest is handed the state version
--                       that wrote it so it can fix it up in place. The guest
--                       asked for this by exporting the hook, rodata and all.
--   changed, fk_migrate the old heap was NOT adopted; the guest is TOLD, on the
--                       fresh heap _initialize built, which is what a
--                       rebuild-from-world needs and is all it needs.
--   changed, neither    the old heap is discarded and the loss is logged by
--                       name. Losing state cleanly beats running a guest on
--                       bytes laid out by a different build, which in a lockstep
--                       game means every client desyncing on whatever the
--                       garbage happened to decode as.
--
-- It is the same function as the deferred path deliberately: two copies drifted
-- once already, which is how the row above spent two milestones saying
-- fk_migrate adopts.
--
-- AND THE HOOK ITSELF IS A GUEST-VISIBLE EVENT SINCE 2026-08-16, because a
-- rebuild is not the only thing this event reports and it was the only thing a
-- guest could hear about. Factorio raises on_configuration_changed when THE MOD
-- SET changes -- another mod added, REMOVED, moved to another version -- when a
-- startup setting moves, and when the game version does. None of those touches
-- this mod's build stamp, so finish_rebuild returns on the flag and the guest is
-- told nothing at all.
--
-- The downstream shape that asked for it: a mod that adopts an incumbent's
-- entities when the incumbent is UNINSTALLED has a once-per-save conversion to
-- run, and the only signal that the neighbour is gone is this event. Without it
-- the best available trigger is "the first event of the session", which converts
-- late and on a tick nobody chose.
--
-- IT IS DISPATCHED UNCONDITIONALLY, and it takes no arguments. What the engine
-- passes -- old_version, new_version, a mod_changes DICTIONARY OF TABLES -- is
-- tier-2 marshalling for a notification, and a guest that wants detail can read
-- script.active_mods and compare against what it saved. The minimum is being
-- TOLD, and being told is the whole of the gap.
--
-- SAFETY: THIS IS REPLICATED, EXACTLY AS fk_migrate IS, and for the same reason
-- rather than by analogy. It runs on the peer that LOADED the save, before the
-- first tick, so its effects are already in the state a joining client
-- downloads -- a joiner never runs it and never has to. So a guest MAY write
-- guest state here, which is what separates it from fk_after_load: that one is
-- armed from script.on_load, which the JOINER runs and nobody else, and a write
-- from there is CLAUDE.md's named desync.
--
-- AFTER finish_rebuild, so a guest that was rebuilt AND had its neighbours
-- change gets fk_migrate first, on a heap that has been settled and republished,
-- and then this. The two answer different questions -- "your bytes are not
-- yours" against "the world around you moved" -- and a guest can want both.
--
-- The dispatch is guarded on the export rather than handed a nil callee, so a
-- guest that does not export it takes exactly the path it took before: no
-- outermost dispatch, no arena bracket, no scratch reset, no sync_memory. The
-- registration is guarded the other way for the same reason -- `persisting` on
-- its own would leave a --persist=none guest that exports the hook unwired.
--
-- AND IT CARRIES ITS PAYLOAD SINCE 2026-08-30, which is the other half of being
-- told. ConfigurationChangedData is a described concept -- old_version,
-- new_version, mod_changes, mod_startup_settings_changed, migration_applied and
-- migrations -- and nothing in the API references it, so no generator had ever
-- emitted it and this hook dispatched with no arguments at all. A guest could
-- hear that SOMETHING moved and never what: which neighbour appeared,
-- disappeared or changed version, and from what.
--
-- ENCODED LIKE AN EVENT, into the same per-level scratch buffer through the same
-- H.write_struct, because it IS an event payload in every way that matters here
-- -- what differs is only that Factorio raises it through a hook rather than
-- through script.on_event, so it has no id and no filters. API.confchanged is
-- the layout, and `fklua mod` packages it only for a guest that exports this
-- hook: there is no id to prune on, so the EXPORT is the key.
--
-- A NO-ARGUMENT GUEST IS UNCHANGED, which is the whole compatibility argument. A
-- wasm export of no parameters compiles to a Lua function of no parameters, and
-- Lua discards extra arguments -- so a guest already in the field takes exactly
-- the path it took before, and one built against older bindings (no
-- API.confchanged, because the table is generated with the package) falls back
-- to the no-argument call rather than passing a pointer into a buffer whose
-- layout it has no reader for.
local function run_config_changed(data)
  local cc = API.confchanged
  if cc == nil or data == nil then
    return E.fk_on_configuration_changed()
  end
  -- depth is already raised, so this names the same per-level buffer run_event
  -- does. A buffer that could not be allocated is not a reason to skip the
  -- notification: the hook still fires, with no payload.
  local buf = event_buffer(depth)
  if buf == 0 and cc.size > 0 then
    return E.fk_on_configuration_changed()
  end
  H.write_struct(cc.fields, buf, data)
  return E.fk_on_configuration_changed(buf)
end

if persisting or E.fk_on_configuration_changed then
  script.on_configuration_changed(function(data)
    finish_rebuild()
    if E.fk_on_configuration_changed then
      dispatch(run_config_changed, data)
    end
  end)
end

-- ---------------------------------------------------------------------------
-- Event wiring.
--
-- Handlers are registered only when the guest actually exports them. on_tick in
-- particular is not free: registering it makes Factorio call into this mod
-- sixty times a second forever, so a guest that does not want it must not pay
-- for it.
--
-- on_init is registered unconditionally under persistence, because the state
-- has to be published to `storage` on a new map whether or not the guest has
-- anything of its own to do there.
-- ---------------------------------------------------------------------------

if E.fk_on_init or persisting then
  script.on_init(function()
    state_init()
    -- dispatch even when the guest has no on_init: the end-of-dispatch work
    -- still has to happen, and a nil callee is the one entry point that can
    -- legitimately be missing.
    dispatch(E.fk_on_init)
  end)
end

if E.fk_on_tick then
  on_event(defines.events.on_tick, function(e)
    -- Invariant A: the guest is handed an unsigned i32. A tick count passes
    -- 2^32 after about two years of continuous play, and wrapping is what the
    -- guest's own uint32 would do.
    dispatch(E.fk_on_tick, e.tick % 4294967296.0)
  end)
end
