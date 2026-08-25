-- FkLua host-call ABI: the handle table.
--
-- `require`d by a packaged mod's control.lua, which is the only place Factorio
-- allows require. Hand-written and copied verbatim, like fk_mod.lua.
--
-- ---------------------------------------------------------------------------
-- WHY HANDLES AT ALL
--
-- wasm has four numeric types and nothing else. A LuaEntity cannot cross into
-- guest memory, so the guest holds an i32 index into a table the host keeps,
-- and every call naming that entity passes the index back.
--
-- ---------------------------------------------------------------------------
-- TWO SPACES, SPLIT AT 0x40000000
--
--   below      PERSISTENT. Allocated only by an explicit fk_retain, freed by
--              fk_release, and kept in `storage` so it survives a save. A guest
--              that stashes an entity across a save gets the entity back rather
--              than a dangling index.
--   at/above   TRANSIENT. Allocated while one event is being dispatched and
--              released WHOLESALE when it returns.
--
-- The transient space is the important half, and it exists to kill the dominant
-- leak class rather than to be fast: a handler reads `event.entity`, does
-- something with it, and drops it. Under a single-space design every one of
-- those leaks an entry forever, and the guest author has no way to know. Here
-- the whole space is discarded at the end of the dispatch that made it, so the
-- default behaviour leaks nothing and `fk_retain` is the only thing an author
-- has to remember -- and only for state they meant to keep.
--
-- Splitting on a bit rather than keeping a flag per entry means "is this
-- persistent" is one compare, and the guest can be handed an opaque number that
-- still tells the host which table to look in.
-- ---------------------------------------------------------------------------

local M = {}

-- The split point. A handle at or above this is transient.
local TRANSIENT = 1073741824      -- 0x40000000

-- 1..9 are the global objects, assigned at load in a fixed order so a guest
-- reaches `game` without a call. See M.bind_globals.
local FIRST_DYNAMIC = 10

-- Status codes. A host call returns one of these to the guest as an i32; it
-- never raises into wasm, because without coroutines there is no way to unwind
-- through a wasm frame. The guest turns a non-zero status into whatever its own
-- language calls an error -- a Go `error`, a Rust `Result`.
M.OK              = 0
M.ERR_BAD_HANDLE  = 1   -- not a live handle
M.ERR_INVALID     = 2   -- a LuaObject whose .valid went false
M.ERR_NO_MEMBER   = 3   -- resolved to nothing on this Factorio version
M.ERR_BAD_ARGS    = 4
M.ERR_CALL_FAILED = 5   -- the API itself raised; message is fetchable
M.ERR_NO_SPACE    = 6

-- Live state. `persistent` is rebound to the storage-backed table in on_load,
-- which is why it is a field rather than an upvalue.
local persistent = {}
local free = {}                   -- reclaimed persistent slots
local nextP = FIRST_DYNAMIC
local transient = {}
local nextT = TRANSIENT

-- genv is where the fixed 1..9 block is READ FROM, not a snapshot of it.
--
-- Resolving lazily rather than binding once is a correctness requirement, not
-- tidiness: `game` DOES NOT EXIST while control.lua is loading, nor inside
-- on_load. Capturing the globals at load time would bind nine nils and every
-- handle 1..9 would be dead for the life of the session. Looking them up on
-- access costs one table read -- the same read a snapshot would have done --
-- and is right at every point in the lifecycle.
local genv = nil

-- ---------------------------------------------------------------------------
-- Allocation
-- ---------------------------------------------------------------------------

-- Hand out a transient handle. The default for anything the host produces.
function M.transient(obj)
  if obj == nil then return 0 end
  local h = nextT
  nextT = nextT + 1
  transient[h] = obj
  return h
end

-- Release every transient handle. Called when a dispatch returns, whatever it
-- returned: a guest that trapped must not keep its handles alive.
function M.clear_transient()
  if nextT == TRANSIENT then return end   -- nothing was handed out
  transient = {}
  nextT = TRANSIENT
end

-- Promote a handle so it outlives the dispatch. Idempotent for one already
-- persistent, which lets a guest retain without first asking which space it is
-- in.
function M.retain(h)
  if h < TRANSIENT then
    if h == 0 then return 0, M.ERR_BAD_HANDLE end
    if h < FIRST_DYNAMIC then
      local name = M.GLOBAL_NAMES[h]
      if name == nil or genv == nil or genv[name] == nil then
        return 0, M.ERR_BAD_HANDLE
      end
      return h, M.OK
    end
    if persistent[h] == nil then return 0, M.ERR_BAD_HANDLE end
    return h, M.OK
  end
  local obj = transient[h]
  if obj == nil then return 0, M.ERR_BAD_HANDLE end

  local p
  local n = #free
  if n > 0 then
    p = free[n]
    free[n] = nil
  else
    p = nextP
    nextP = nextP + 1
    if p >= TRANSIENT then return 0, M.ERR_NO_SPACE end
  end
  persistent[p] = obj
  return p, M.OK
end

-- Free a persistent handle. Releasing a transient one is not an error and does
-- nothing: it is about to be released anyway, and making that a failure would
-- force guests to track which space a handle came from.
function M.release(h)
  if h >= TRANSIENT then return M.OK end
  if h < FIRST_DYNAMIC then return M.ERR_BAD_HANDLE end  -- globals are not owned
  if persistent[h] == nil then return M.ERR_BAD_HANDLE end
  persistent[h] = nil
  free[#free + 1] = h
  return M.OK
end

-- ---------------------------------------------------------------------------
-- Lookup
-- ---------------------------------------------------------------------------

-- Resolve a handle, or return nil and a status.
--
-- The `.valid` check is the one that stops a whole class of crash. Factorio
-- invalidates a LuaObject when the thing behind it is destroyed, and touching
-- an invalid one raises a Lua error -- which, from inside a guest call, would
-- unwind past wasm frames that cannot be unwound. Better to hand back a status
-- the guest can act on.
function M.get(h)
  if h == 0 then return nil, M.ERR_BAD_HANDLE end
  local obj
  if h >= TRANSIENT then
    obj = transient[h]
  elseif h < FIRST_DYNAMIC then
    local name = M.GLOBAL_NAMES[h]
    obj = name ~= nil and genv ~= nil and genv[name] or nil
  else
    obj = persistent[h]
  end
  if obj == nil then return nil, M.ERR_BAD_HANDLE end
  return obj, M.OK
end

-- Whether a live object is still usable.
--
-- NOT FOLDED INTO get(), AND THIS COST A TRIP THROUGH THE REAL GAME TO LEARN.
-- 15 of 148 classes have no `valid` attribute -- including six of the nine
-- globals -- and reading a key a LuaObject does not have does NOT return nil.
-- Factorio's __index RAISES: "LuaGameScript doesn't contain key valid." So a
-- blanket `obj.valid == false` probe crashed every call on `game`.
--
-- The caller passes whether this member's class has the attribute, which the
-- generator knows from the API description. That makes the check free where it
-- does not apply and correct where it does, with no pcall on the hot path.
function M.check_valid(obj, has)
  if has and obj.valid == false then return M.ERR_INVALID end
  return M.OK
end

-- ---------------------------------------------------------------------------
-- Globals and persistence
-- ---------------------------------------------------------------------------

-- ORDER IS THE ABI. A guest compiled against this list uses the numbers, so
-- appending is safe and reordering is not. It matches runtime-api.json's
-- global_objects, alphabetically, which is the order that file lists them in.
M.GLOBAL_NAMES = {
  "commands", "game", "helpers", "prototypes", "rcon",
  "remote", "rendering", "script", "settings",
}

-- Say where the globals live. `env` is _G in a mod and a stub in tests. Nothing
-- is read here -- see genv.
function M.bind_globals(env) genv = env end

-- Adopt the persistent space from a save.
--
-- LuaObject references are among the things Factorio will serialize into
-- `storage`, which is what makes a persistent handle survive a save at all. The
-- free list is rebuilt rather than stored: it is derivable, and a stale one
-- read back from a save would hand out a slot that is still in use.
function M.adopt(saved)
  persistent = saved or {}
  free = {}
  nextP = FIRST_DYNAMIC
  for h in pairs(persistent) do
    if h >= nextP then nextP = h + 1 end
  end
  for h = FIRST_DYNAMIC, nextP - 1 do
    if persistent[h] == nil then free[#free + 1] = h end
  end
end

-- The table to hand to `storage`. It IS the live table, so a later retain lands
-- in the saved structure with no sync step -- the same aliasing trick
-- --persist=table uses for guest memory.
function M.persistent_table() return persistent end

-- ---------------------------------------------------------------------------
-- Member dispatch
--
-- ONE generic import rather than 3283 of them:
--
--   fk.call(handle, member, argp, retp) -> status
--
-- Why not one import per member: a method removed in a Factorio point release
-- becomes an unresolved import, and an unresolved import means the WHOLE MODULE
-- fails to instantiate. Given that `latest` is routinely ahead of a typical
-- install, that is the difference between "one call returns ERR_NO_MEMBER with a
-- diagnostic" and "your mod is broken for everyone, at load, silently".
--
-- What a member entry needs is smaller than it first looks. Reading `obj[name]`
-- and calling `obj[name](...)` are generic over every class, so dispatch
-- needs no per-class code at all -- specialisation belongs to MARSHALLING,
-- which knows the argument types. This layer is deliberately value-based:
-- argp/retp encoding sits on top of it, and separating the two keeps the codec
-- testable without a wasm module and this testable without a codec.
-- ---------------------------------------------------------------------------

-- Member kinds.
M.CALL = 0        -- obj:method(...)
M.GET  = 1        -- obj.attr
M.SET  = 2        -- obj.attr = v
M.EQ   = 3        -- obj.attr == want, compared HERE, returning a bool
M.IDX  = 4        -- obj[k]      the __index operator
M.LEN  = 5        -- #obj        the __len operator
M.SELF = 6        -- obj(...)    the __call operator
-- GETH reads obj.attr and returns the OBJECT rather than a copy of what is in
-- it, and it exists for exactly one shape: an attribute whose type is a
-- LuaCustomTable. Nothing in the API RETURNS one, so the index and length
-- operators bound on that class had no reachable receiver -- and the
-- materialising read that stood in for them copied all 319 of
-- force.technologies across the boundary for one lookup.
--
-- It is a kind rather than a branch because everything that differs is in the
-- declared RETURN KIND, which write_value has always dispatched on: K_HANDLE
-- where the GET member says K_DICT. The read itself is identical, which is why
-- the two share a line below.
M.GETH = 7        -- obj.attr, as a handle
-- IDXSET is the WRITE half of IDX: `obj[k] = v`, with the key AND the value
-- both arguments. It is the only way a mod can change its own runtime-global
-- setting -- `settings.global["name"] = {value = true}` -- which is a gesture
-- no other kind can express, because SET takes its member name from the
-- generation-time member table and IDX has nowhere to put a value.
--
-- THE DESCRIPTION DOES NOT MODEL IT. An operator carries a `read_type` and
-- never a `write_type`, so no generator that mirrors the description can emit
-- this; what the description DOES carry is PROSE, on the operator itself
-- ("Access, set or clear a fluid box... Writing `nil` removes all fluid") and
-- on the members that yield a LuaCustomTable ("individual settings can be
-- changed by overwriting their ModSetting table"). The generator reads that
-- prose through an explicit allowlist -- see indexWriteHalf in
-- internal/factorio/gen.go -- so which receivers accept a write is a decision
-- written down once rather than a shape guessed per class.
--
-- WRITABILITY IS PER RECEIVER AND NOT PER CLASS, which is why the refusal is
-- left to the engine. `settings.global` accepts a write and `settings.startup`
-- answers "LuaCustomTable is read only"; both are the same LuaCustomTable and
-- therefore the same member id. That raise comes back as ERR_CALL_FAILED with
-- the engine's own text in last_error, exactly like every other raise here.
M.IDXSET = 8      -- obj[k] = v  the __newindex operator
-- GFUNC is a function on NO CLASS: Factorio's three globals, `log`,
-- `localised_print` and `table_size`. It is the one kind whose branch runs
-- BEFORE the handle is resolved, because there is no receiver to resolve -- the
-- generated binding passes 0 and nothing here reads it.
--
-- IT EXISTS BECAUSE `log` IS THE ONLY WAY TO READ A LuaProfiler'S DURATION.
-- LuaProfiler's complete member set is add, divide, reset, restart, stop,
-- object_name, object_name_is and valid: not one of them returns the number.
-- The engine renders it only when the profiler is an ELEMENT OF A
-- LocalisedString -- `log{"", "took ", p}` -- so a guest that cannot call log
-- cannot time anything and read the answer. Downstream (BetterBeltBalancer)
-- regexes exactly that out of factorio-current.log for every timing figure it
-- publishes, and `global_functions_bound: 0` in census.json is the
-- written-down zero that came due.
--
-- WHERE THE FUNCTION COMES FROM is `genv`, the same lazily-read environment the
-- fixed 1..9 handle block resolves against, and for the same reason: capturing
-- at load time would bind whatever existed while control.lua was still running.
-- A global the running Factorio does not have is ERR_NO_MEMBER through
-- report_missing, exactly as a removed member of a class is -- which is what
-- generic dispatch buys and what a per-function import would not.
M.GFUNC = 9       -- log(...), localised_print(...), table_size(...)
--
-- EQ exists so a guest can ask `entity.name == "transport-belt"` WITHOUT the
-- name ever crossing into guest memory. The comparison is one Lua `==` on a
-- string the host already holds; what it removes is the other direction --
-- fk_alloc into the guest, fk_wstr writing the bytes, and the guest copying
-- them into a language string it then throws away. Downstream measured that at
-- 32 B of permanent guest heap per build event under -gc=leaking, on the one
-- path no mod can avoid: it has to read the name to find out it does not care.
--
-- It is a KIND and not a special import for the same reason GET and SET are
-- kinds: everything above -- handle resolution, the `valid` check, the pcall
-- around the member read, ERR_NO_MEMBER -- is shared by construction, and
-- fk.call needs no new shape.
--
-- IDX, LEN and SELF are Lua's three CLASS OPERATORS, and they are kinds for the
-- same reason again. Eleven of them exist across seven classes -- LuaInventory's
-- `inv[1]`, LuaCustomTable's `t[name]`, LuaChunkIterator's `it()` -- and until
-- 2026-08-03 no generator read them, so those classes bound `valid` and
-- `object_name` and nothing that made them useful. Reported by fklua-ports'
-- resource-marker (RM1), qol-research (Q2) and fluid-memory-storage (F-IDX),
-- which turned out to be one gap seen from three sides.
--
-- WHAT MAKES THEM NEED KINDS AT ALL rather than riding on GET: every kind above
-- begins by resolving `obj[m.name]`, and none of these three is a member read.
-- `obj[k]` indexes with an ARGUMENT; `#obj` is a length; `obj(...)` calls the
-- object. So each gets its own branch in M.invoke -- two lines apiece -- and
-- shares everything before it. `m.name` still travels for these, and it is
-- documentation and diagnostics only: nothing ever resolves it.
--
-- IDXSET is a fourth for the same reason and not for SET's: SET's value is its
-- only argument and its NAME is in the member table, where IDXSET's key is an
-- argument too. That is the same sentence that made IDX a kind, one direction
-- over.
--
-- GFUNC is a fifth, and it is the one that needs LESS than the others rather
-- than more: no handle, no `valid` check, no member read. It is placed above
-- all of them in M.invoke for exactly that reason.

local members = {}
local lastError = ""
-- Names already reported missing. A guest that calls a removed member every
-- tick would otherwise fill the log at sixty lines a second, which buries the
-- one line that mattered.
local reported = {}

-- Install the member table.
--
-- `list[i] = { kind = M.CALL, name = "destroy" }`, and i IS the member id the
-- guest was compiled with. Ids are per-BUILD and dense rather than global: the
-- generator assigns them from the manifest of members that guest actually
-- references, so adding a member to Factorio cannot renumber anything.
function M.bind_members(list)
  members = list or {}
  reported = {}
end

-- The message from the last ERR_CALL_FAILED. Fetched separately because a
-- status is an i32 and a message is not.
--
-- IT DESCRIBES THE HOST CALL THAT JUST RETURNED, WHICH IS WHY M.call CLEARS IT
-- ON THE WAY IN. Without that it would mean "whatever failed last, ever" -- and
-- a guest reading it after a call that SUCCEEDED would get a stale sentence
-- about some other member, which reads exactly like a fresh one. Clearing at
-- the single entry point is one upvalue store per host call and makes the
-- contract sayable in a line: after a call returns ERR_CALL_FAILED this is THAT
-- call's message, and after any other outcome it is empty.
--
-- The one seam is RE-ENTRANCY: invoke can raise an event synchronously, whose
-- handler makes host calls of its own, so an inner failure under an outer
-- SUCCESS leaves the inner message standing. That is the honest answer -- the
-- inner call really is the last one that failed -- and it costs nothing to a
-- guest reading this where it is meant to be read.
--
-- `fk.last_error` is the import that carries it into guest memory; see
-- fk_mod.lua. Until that existed the only consumers were this repo's own tests,
-- so a guest could see ERR_CALL_FAILED and never what the engine said -- which
-- is the difference between "the API refused" and "the API refused BECAUSE",
-- and downstream needs the second to assert a refusal is still the refusal it
-- was.
function M.last_error() return lastError end

-- Hoisted so pcall receives a plain function value: building a closure per host
-- call would allocate on the hot path, in a lockstep game loop.
local function rawget_member(obj, name) return obj[name] end
local function set_member(obj, name, v) obj[name] = v end

-- The three class operators, hoisted for the same reason: pcall wants a plain
-- function value, and building a closure per host call allocates on the hot
-- path of a lockstep game loop.
--
-- index_at is `obj[k]` where k is an ARGUMENT rather than a member name, which
-- is what rawget_member above cannot express and why IDX is a kind. len_of is
-- `#obj`. There is no call_self: a LuaObject with a __call metamethod is
-- already a callable value, so pcall takes it directly.
--
-- index_set is `obj[k] = v`, the write half, and it is a THIRD function rather
-- than a reuse of set_member for the same reason index_at is not
-- rawget_member: the key is an argument here and a member name there.
local function index_at(obj, k) return obj[k] end
local function len_of(obj) return #obj end
local function index_set(obj, k, v) obj[k] = v end

local function report_missing(name)
  if reported[name] then return end
  reported[name] = true
  if log then
    log("fklua: this Factorio does not have " .. tostring(name) ..
        ", so calls to it return ERR_NO_MEMBER. The mod keeps running.")
  end
end

-- Invoke a member on a handle.
--
-- Returns a status followed by whatever the API returned. Every failure path is
-- a STATUS: a host call never raises into wasm, because without coroutines
-- there is no way to unwind a wasm frame, and an error crossing that boundary
-- would take down the whole mod rather than the one call.
function M.invoke(h, mid, ...)
  local m = members[mid]
  if m == nil then return M.ERR_NO_MEMBER end

  -- A GLOBAL FUNCTION HAS NO RECEIVER, so this branch comes before the handle
  -- is resolved rather than after. `h` is not read at all -- the generated
  -- binding passes 0, which every other kind would answer ERR_BAD_HANDLE.
  --
  -- genv rather than a captured upvalue, and `type(f) == "function"` rather
  -- than a nil test: a Factorio that does not have this global reads as a
  -- missing member, reported once and answered with a status every time after,
  -- which is the same degradation a removed class member gets. There is no
  -- pcall around the READ because `_G` is a plain table with no metamethod --
  -- the pcall that matters is around the CALL, for the same reason it wraps
  -- every other one here: a Lua error crossing a wasm frame takes the mod down
  -- rather than the call.
  if m.kind == M.GFUNC then
    local f = genv ~= nil and genv[m.name] or nil
    if type(f) ~= "function" then
      report_missing(m.name)
      return M.ERR_NO_MEMBER
    end
    -- Four result slots, matching the CALL kind: table_size returns one and the
    -- other two return nothing, and sharing the shape costs nothing.
    local gok, ga, gb, gc, gd = pcall(f, ...)
    if not gok then
      lastError = tostring(ga)
      return M.ERR_CALL_FAILED
    end
    return M.OK, ga, gb, gc, gd
  end

  local obj, st = M.get(h)
  if obj == nil then return st end
  st = M.check_valid(obj, m.valid)
  if st ~= M.OK then return st end

  -- THE CLASS OPERATORS COME FIRST, BEFORE THE MEMBER READ, and that ordering
  -- is the whole of what they need. None of the four is `obj[m.name]`, so
  -- falling through to the read below would resolve a key called "index" or
  -- "length" on a LuaObject -- which raises on some classes and returns nil on
  -- the rest, i.e. ERR_NO_MEMBER for a member that is right there.
  --
  -- Each is pcall-ed for exactly the reason the member read is: a Factorio
  -- metamethod raises (an inventory index out of range, an iterator past its
  -- end), and an error crossing a wasm frame takes down the mod rather than the
  -- call. A nil result is NOT an error here -- LuaFluidBox's index is declared
  -- optional and an empty fluid box really is nil -- so it goes back through
  -- encode_rets, which clears the presence byte the signature carries.
  if m.kind == M.IDX then
    local iok, v = pcall(index_at, obj, ...)
    if not iok then
      lastError = tostring(v)
      return M.ERR_CALL_FAILED
    end
    return M.OK, v
  end

  if m.kind == M.IDXSET then
    -- `obj[k] = v`, the write half of IDX. Both are bound to names first: `...`
    -- inside a non-vararg closure is a Lua syntax error rather than a capture
    -- of the enclosing varargs, which is the same line M.SET below carries.
    --
    -- AN ABSENT VALUE IS A REAL nil AND IS THE POINT, not an omission to guard
    -- against: M.call trims to the last argument PRESENT, so a member whose
    -- value the description declares optional -- LuaFluidBox's, whose own prose
    -- says "Writing `nil` removes all fluid from the fluid box" -- arrives here
    -- with v nil and clears the slot. Nothing to special-case; the general rule
    -- already says it.
    --
    -- NOTHING COMES BACK. An assignment is not an expression in Lua and this
    -- ABI does not invent one: a caller who wants to know what is there now
    -- asks IDX, which is a second host call it did not have to make.
    local k, v = ...
    local iok, ierr = pcall(index_set, obj, k, v)
    if not iok then
      lastError = tostring(ierr)
      return M.ERR_CALL_FAILED
    end
    return M.OK
  end

  if m.kind == M.LEN then
    local lok, v = pcall(len_of, obj)
    if not lok then
      lastError = tostring(v)
      return M.ERR_CALL_FAILED
    end
    return M.OK, v
  end

  if m.kind == M.SELF then
    -- pcall(obj, ...) rather than pcall(f, ...): the object IS the callable,
    -- and there is no member to look up. Four result slots, matching the CALL
    -- kind below -- no operator in the API returns more than one, and sharing
    -- the shape costs nothing.
    local cok, a, b, c, d = pcall(obj, ...)
    if not cok then
      lastError = tostring(a)
      return M.ERR_CALL_FAILED
    end
    return M.OK, a, b, c, d
  end

  -- READING THE MEMBER IS ITSELF GUARDED. obj[name] is not a plain table lookup:
  -- a LuaObject has an __index metamethod, and Factorio raises from it for some
  -- accesses. Reading outside the pcall lets that error unwind straight through
  -- the wasm frame this call came from -- which is the one thing this whole
  -- layer exists to prevent.
  local ok, f = pcall(rawget_member, obj, m.name)
  if not ok then
    lastError = tostring(f)
    return M.ERR_CALL_FAILED
  end

  -- Existence is checked HERE rather than at load, and that is a departure from
  -- the plan worth knowing about: Lua cannot be asked whether a class has a
  -- member without an instance of it, and the mod does not ship the API
  -- description it was built against. So a removed member is discovered on
  -- first call, reported once, and returns a status every time after. A SET is
  -- exempt: assigning a field that reads as nil is how you create it.
  --
  -- AND SO IS A MEMBER THE DESCRIPTION DECLARES OPTIONAL, which is `opt` and is
  -- the whole of the second half of this fix. runtime-api.json marks 666
  -- readable attributes `optional: true` -- LuaEntity.temperature is present on
  -- a reactor and absent on a chest -- and for those, nil is a VALUE rather than
  -- evidence about this Factorio's version. Without the flag they came back as
  -- ERR_NO_MEMBER, "no such member on this Factorio version", which is the same
  -- status a member genuinely REMOVED in a point release produces: a guest could
  -- not tell "this chest has no temperature" from "this Factorio has no such
  -- attribute", and that distinction is the only reason the status exists.
  -- Reported downstream (fklua-ports-samples, Q4).
  --
  -- The flag comes from the generator rather than being inferred here, and that
  -- is what keeps the distinction rather than erasing it: nil still means
  -- ERR_NO_MEMBER everywhere the description did not say optional, which is
  -- every method and the 3,000-odd attributes that are not.
  --
  -- Two consumers, both below. A GET returns OK with nil, and encode_rets
  -- clears the presence byte its signature now carries. An EQ falls through to
  -- call_eq, whose `type(f) == "string"` already answers false -- an absent
  -- attribute is not equal to the string being asked about, which is an answer
  -- and not a failure.
  if f == nil and m.kind ~= M.SET and not m.opt then
    report_missing(m.name)
    return M.ERR_NO_MEMBER
  end

  if m.kind == M.GET or m.kind == M.GETH then
    -- ONE LINE FOR BOTH: the read is the same and only the declared return kind
    -- differs, so encode_rets writes a handle for one and a materialised
    -- dictionary for the other out of the very same Lua value.
    return M.OK, f                      -- already read, and read safely
  end

  if m.kind == M.EQ then
    -- EQ RETURNS THE STRING, EXACTLY AS GET DOES, and M.call does the compare.
    --
    -- That looks like the wrong place for it and is the right one: the compare
    -- needs the guest's (ptr, len) BEFORE the string it points at has been
    -- decoded, so that a length mismatch can answer without decoding anything.
    -- Doing it here would mean decode_args had already run, which is the cost
    -- the fast path exists to skip. See M.call.
    return M.OK, f
  end

  if m.kind == M.SET then
    -- The value is bound to a name first: `...` inside a non-vararg closure is
    -- a Lua syntax error, not a capture of the enclosing varargs.
    local v = ...
    local sok, serr = pcall(set_member, obj, m.name, v)
    if not sok then
      lastError = tostring(serr)
      return M.ERR_CALL_FAILED
    end
    return M.OK
  end

  if type(f) ~= "function" then
    report_missing(m.name)
    return M.ERR_NO_MEMBER
  end
  -- NO `obj` HERE. A Factorio LuaObject's __index returns a closure ALREADY
  -- BOUND to the object -- which is why every line of real mod code reads
  -- `surface.create_entity{...}`, dot-called and never colon-called -- and the
  -- engine's argument checker counts what arrives exactly. Passing the object a
  -- second time is one argument too many on EVERY method in the API:
  -- "Arguments count error for '?': Expected 1 argument but 2 were given".
  --
  -- Four result slots against a measured maximum of THREE return values across
  -- all 960 methods (TestMethodReturnArity pins it), so this cannot silently
  -- truncate. Named slots rather than table.pack because packing would allocate
  -- on every host call, inside a lockstep game loop.
  local cok, a, b, c, d = pcall(f, ...)
  if not cok then
    lastError = tostring(a)
    return M.ERR_CALL_FAILED
  end
  return M.OK, a, b, c, d
end

-- ---------------------------------------------------------------------------
-- Marshalling: the wire between guest memory and Lua values
--
-- fk.call(handle, member, argp, retp) -> status. argp and retp point at blocks
-- in the GUEST's linear memory, laid out exactly as a C struct would be: each
-- field aligned to its own size, in declaration order. The generator emits the
-- matching struct on the guest side, so both ends agree by construction rather
-- than by comment.
--
-- Every value here obeys Invariant A. An i32 arriving from the guest is an
-- UNSIGNED double in [0, 2^32), so a signed field needs an explicit fold at the
-- boundary -- and forgetting that is a wrong number, not a crash, which is why
-- the fold lives in one place per width instead of at each use.
-- ---------------------------------------------------------------------------

-- Field kinds. Numbers rather than strings: a member signature is walked on
-- every host call, and a string compare per field is a cost with no payoff.
M.K_I8, M.K_U8   = 1, 2
M.K_I16, M.K_U16 = 3, 4
M.K_I32, M.K_U32 = 5, 6
M.K_F32, M.K_F64 = 7, 8
M.K_BOOL         = 9
M.K_STR          = 10   -- ptr + len, two i32 slots
M.K_HANDLE       = 11   -- an i32 index into this table
M.K_U64          = 12   -- lo + hi, two i32 slots
M.K_STRUCT       = 13   -- a nested named-field block
M.K_ARRAY        = 14   -- ptr + count, elements out of line
M.K_DICT         = 15   -- ptr + count over key/value PAIRS
M.K_DYN          = 16   -- tier 2: a self-describing tagged value

-- Bytes each kind occupies, and the alignment it demands.
local WIDTH = {
  [1] = 1, [2] = 1, [3] = 2, [4] = 2, [5] = 4, [6] = 4,
  [7] = 4, [8] = 8, [9] = 1, [10] = 8, [11] = 4, [12] = 8,
  [14] = 8, [15] = 8, [16] = 16,
}
-- A string is two i32s and a u64 is two i32s, so both align to 4 rather than to
-- their total width. A dynamic value aligns to 8 because its payload slot holds
-- an f64. Anything else aligns to its own size.
local ALIGN = { [10] = 4, [12] = 4, [14] = 4, [15] = 4, [16] = 8 }

function M.field_width(k) return WIDTH[k] end
function M.field_align(k) return ALIGN[k] or WIDTH[k] end

local io_                      -- the module's memio, bound below
local alloc_, free_            -- the guest's own allocator

-- The memio accessors, cached as module locals rather than reached through
-- io_ on every use.
--
-- `io_.ld32(at)` is a HASH LOOKUP followed by a call. Tier 2 does one to three
-- of those per value and recurses per element, so a six-key map argument makes
-- ~40 of them -- and the lookup is pure overhead, because io_ is rebound only
-- by bind_memory. These are set there, so the two can never disagree; nothing
-- outside the tier-2 codec uses them, which keeps the change to the path that
-- was measured.
local ld8_, ld32_, ldf64_, st8_, st32_, stf64_

-- Give the ABI access to guest memory. `m` is the generated module's `memio`,
-- whose closures capture MEM by reference and so survive grow and adopt.
function M.bind_memory(m)
  io_ = m
  ld8_, ld32_, ldf64_ = m.ld8, m.ld32, m.ldf64
  st8_, st32_, stf64_ = m.st8, m.st32, m.stf64
end

-- Bind the guest's allocator.
--
-- A string the host returns needs somewhere in GUEST memory to live, and only
-- the guest can say where: its allocator owns that address space, and a pointer
-- the host invented would land in the middle of something. So the guest exports
-- fk_alloc(n) -> ptr and fk_free(ptr), and a binding that returns a string is
-- the one shape that cannot work without them.
--
-- OWNERSHIP: the host allocates, the GUEST frees. The generated binding copies
-- the bytes into its own language's string type and calls fk_free -- which is
-- the only point at which anything knows the value is finished with.
function M.bind_alloc(a, f) alloc_, free_ = a, f end

-- The string scratch region: a fixed block of guest memory the host writes
-- returned strings into instead of calling fk_alloc for each one.
--
-- WHY IT IS SOUND. bind_alloc's contract above says the host allocates and the
-- GUEST frees, because the generated binding copies the bytes into its own
-- language's string type and is done with the pointer. The lifetime is
-- therefore call-scoped, which is exactly what a reusable region serves --
-- fk_mod.lua already relies on the same property for event data, where the
-- scratch buffer means "a dispatch allocates nothing".
--
-- WHY IT IS WORTH IT. A real fk_alloc is a //go:wasmexport whose body is
-- make([]byte, n), compiled to Lua. Measured against the Lua closure the ABI
-- cost test binds: 1535 ns against 53 for one alloc+free pair, and alloc is
-- ~1333 of that. It was ~53% of a real string return.
local scratchBase, scratchSize, scratchTop = 0, 0, 0

-- Bound from fk_mod.lua out of the guest's own exports, so the GUEST still
-- owns the address -- the same reason bind_alloc exists rather than the host
-- picking a pointer. Two exports rather than one returning a pair, because
-- multivalue is not in the feature set FkLua compiles.
function M.bind_scratch(base, size)
  scratchBase, scratchSize, scratchTop = base or 0, size or 0, 0
end

-- Reclaim the whole region. Called by fk_mod.lua at the OUTERMOST dispatch
-- only, which is the one point at which nothing the host wrote is still being
-- read by anybody.
function M.scratch_reset() scratchTop = 0 end

-- Where the region currently stands, and a way back to it.
--
-- THIS IS THE RE-ENTRANCY MECHANISM AND IT IS NOT OPTIONAL. Factorio raises
-- some events synchronously from inside the API call that caused them, so the
-- order of events is: an event's string fields are written into the region, the
-- guest handler starts reading them, and the handler makes its OWN host calls
-- before it is finished. A single reset-to-zero at the top of encode_rets would
-- write the handler's return values straight over the event fields it is still
-- reading -- the same shape as the scratch-buffer bug the audit found, and just
-- as invisible, because the bytes are structurally valid and merely belong to
-- something else.
--
-- So a call reclaims only back to where IT started, never to zero: whatever is
-- below its mark belongs to something further out that is still live.
function M.scratch_mark() return scratchTop end
function M.scratch_release(mark) scratchTop = mark end

-- Signed folds. An i32 crosses as unsigned, so a negative one arrives as
-- 2^32 + v and has to be folded back exactly once.
local function fold(v, half, whole)
  if v >= half then return v - whole end
  return v
end

-- Forward declarations. Every aggregate can contain every other, so the whole
-- set is mutually recursive and read_value/write_value are the single point
-- that dispatches on kind -- without them each container would repeat the same
-- four-way branch, and adding a kind would mean finding all of them.
local read_field, write_field, read_struct, write_struct
local read_value, write_value, read_array, write_array, read_dyn, write_dyn

-- ---------------------------------------------------------------------------
-- Tier 2: the tagged codec
--
-- ONE codec instead of 93 generated union types. A structural union has no
-- fixed layout, and LocalisedString is defined in terms of itself -- neither
-- shape survives a struct. A tag saying what is ACTUALLY there carries both,
-- and tolerates version skew for free: the tag describes the value rather than
-- what the schema said the value would be.
--
-- 16 bytes. Tag at 0, payload at 8: an f64, a (ptr, len) string, a handle, or a
-- (ptr, count) over more of these.
-- ---------------------------------------------------------------------------

-- The tags, as module LOCALS with M.* aliases beside them. Both spellings
-- exist because both are needed: the alias is the cross-language contract
-- TestKindNumbersMatchTheLuaABI reads out of this file, and the local is what
-- the codec compares against -- `M.DYN_STR` in a branch is a hash lookup on M,
-- and the decode ran up to seven of them per value before finding its tag.
-- Container payloads for tier-2 values: the string scratch region first, the
-- guest allocator second.
--
-- IT IS THE SAME POLICY write_field's K_STR path has had all along, applied to
-- the one place that did not have it, and it is a correctness requirement rather
-- than an optimisation for exactly one caller. A tier-2 value written as a
-- RETURN is bracketed by the guest binding that made the call, which takes an
-- arena mark before and releases it after -- so alloc_ there is call-scoped and
-- reclaimed. A tier-2 value written as an ARGUMENT to a host-initiated dispatch
-- (a command handler, a remote interface method -- see fk_mod.lua's trampolines)
-- has no such bracket: nothing on the guest side made the call, so nothing on
-- the guest side releases it, and every invocation would leak the payload for
-- the life of the mod. The scratch region is reclaimed by fk_mod.lua at every
-- OUTERMOST dispatch, which is precisely the lifetime a trampoline's arguments
-- want.
--
-- The arena fallback stays for the case the region cannot hold, which is what
-- keeps a large value correct rather than refused; on the trampoline path that
-- is a bounded residual rather than a leak that grows with time, because it
-- needs one invocation whose arguments exceed the whole 4 KiB region.
local function dyn_alloc(n)
  if scratchSize > 0 and scratchTop + n <= scratchSize then
    local p = scratchBase + scratchTop
    scratchTop = scratchTop + n
    return p
  end
  if alloc_ == nil then return 0 end
  local p = alloc_(n)
  if p == nil then return 0 end
  return p
end

-- Free only what the ALLOCATOR owns. Handing a scratch offset to fk_free would
-- have it scan its pin list for an address that was never pinned -- the same
-- distinction write_field's K_STR path draws in its second return value.
local function dyn_free(p)
  if p == 0 then return end
  if scratchSize > 0 and p >= scratchBase and p < scratchBase + scratchSize then
    return
  end
  if free_ then free_(p) end
end

local DYN_NIL, DYN_BOOL, DYN_NUM, DYN_STR, DYN_OBJ, DYN_ARR, DYN_MAP =
  0, 1, 2, 3, 4, 5, 6
M.DYN_NIL, M.DYN_BOOL, M.DYN_NUM = 0, 1, 2
M.DYN_STR, M.DYN_OBJ = 3, 4
M.DYN_ARR, M.DYN_MAP = 5, 6

local DYNW = 16          -- one dynamic value
local DYNPW = 32         -- one key/value pair of them

-- Exported because fk_mod.lua's callback trampolines size their own buffers in
-- these units -- two slots, the arguments and the result. Keeping the number
-- here rather than repeating it there is the same rule the field WIDTH table
-- follows: one statement of the geometry, in the file that owns it.
M.DYNW = DYNW
M.DYNPW = DYNPW

-- THE BRANCH ORDER IS BY MEASURED FREQUENCY, not by tag number.
--
-- A tag is decoded with a compare chain rather than a dispatch table because
-- the table costs a hash lookup plus a closure call where this costs one to
-- three integer compares -- but that is only true if the common tags come
-- first. What a real tier-2 argument is made of, measured on a
-- create_entity-shaped map, is strings, numbers and maps; nil, bool and object
-- are the tail. The old order was the tag numbering, which put nil and bool
-- first because they were declared first.
function read_dyn(at)
  local tag = ld32_(at)
  if tag == DYN_STR then
    local n = ld32_(at + 12)
    if n == 0 then return "" end
    return M.read_string(ld32_(at + 8), n)
  end
  if tag == DYN_NUM then return ldf64_(at + 8) end
  if tag == DYN_MAP then
    local ptr, n = ld32_(at + 8), ld32_(at + 12)
    local out = {}
    for i = 0, n - 1 do
      local e = ptr + i * DYNPW
      out[read_dyn(e)] = read_dyn(e + DYNW)
    end
    return out
  end
  if tag == DYN_ARR then
    local ptr, n = ld32_(at + 8), ld32_(at + 12)
    local out = {}
    for i = 0, n - 1 do out[i + 1] = read_dyn(ptr + i * DYNW) end
    return out
  end
  if tag == DYN_BOOL then return ld8_(at + 8) ~= 0 end
  if tag == DYN_OBJ then return (M.get(ld32_(at + 8))) end
  return nil                        -- DYN_NIL, and any tag this build lacks
end

-- Is this a LuaObject rather than a plain table?
--
-- It cannot be decided by reading a key, because a key a LuaObject does not
-- have RAISES -- the same trap that broke the `valid` probe. object_name is on
-- 142 of the 148 classes, and the pcall is affordable HERE where it would not
-- be on the tier-1 path: tier 2 is the general, slower road by construction.
local function object_name_of(v)
  local ok, name = pcall(function() return v.object_name end)
  if ok and type(name) == "string" then return name end
  return nil
end

-- The mirror of read_dyn, and ordered the same way and for the same reason:
-- what a real event payload or tier-2 argument is made of is strings, numbers
-- and tables. `nil` keeps its place at the front only because the type() of nil
-- is not one of the names below.
function write_dyn(at, v)
  local t = type(v)
  if v == nil then
    st32_(at, DYN_NIL)
    return M.OK
  end
  if t == "string" then
    st32_(at, DYN_STR)
    return write_field(M.K_STR, at + 8, v)
  end
  if t == "number" then
    st32_(at, DYN_NUM)
    stf64_(at + 8, v)
    return M.OK
  end
  if t == "boolean" then
    st32_(at, DYN_BOOL)
    st8_(at + 8, v and 1 or 0)
    return M.OK
  end
  if t ~= "table" and t ~= "userdata" then
    -- A function crossing into a guest has nowhere to live; nil is the honest
    -- answer rather than a handle to something the guest cannot call.
    st32_(at, DYN_NIL)
    return M.OK
  end
  if object_name_of(v) ~= nil then
    st32_(at, DYN_OBJ)
    st32_(at + 8, M.transient(v))
    return M.OK
  end

  -- A table is an array when it has a positive length and nothing else, which
  -- is the shape the API uses for a LocalisedString's parameter list.
  local n = #v
  local total = 0
  for _ in pairs(v) do total = total + 1 end
  if n > 0 and n == total then
    local ptr = dyn_alloc(n * DYNW)
    if ptr == 0 then return M.ERR_NO_SPACE end
    for i = 1, n do
      local st = write_dyn(ptr + (i - 1) * DYNW, v[i])
      if st ~= M.OK then
        dyn_free(ptr)
        return st
      end
    end
    st32_(at, DYN_ARR)
    st32_(at + 8, ptr)
    st32_(at + 12, n)
    return M.OK
  end

  if total == 0 then
    -- An empty table is an empty array; nothing distinguishes the two and the
    -- guest can tell as much from the count.
    st32_(at, DYN_ARR)
    st32_(at + 8, 0)
    st32_(at + 12, 0)
    return M.OK
  end

  local ptr = dyn_alloc(total * DYNPW)
  if ptr == 0 then return M.ERR_NO_SPACE end
  local i = 0
  for k, val in pairs(v) do
    local e = ptr + i * DYNPW
    local st = write_dyn(e, k)
    if st == M.OK then st = write_dyn(e + DYNW, val) end
    if st ~= M.OK then
      dyn_free(ptr)
      return st
    end
    i = i + 1
  end
  st32_(at, DYN_MAP)
  st32_(at + 8, ptr)
  st32_(at + 12, total)
  return M.OK
end

-- Read a named-field block into a Lua table.
--
-- OPTIONAL FIELDS ARE OMITTED, not set to nil-equivalent defaults. The Factorio
-- API distinguishes "absent" from "present and false" all over -- an absent
-- optional means "leave it alone", a present false means "turn it off" -- so
-- writing a default would change what the call does.
function read_struct(fields, base)
  local t = {}
  for i = 1, #fields do
    local f = fields[i]
    if f.has == nil or io_.ld8(base + f.has) ~= 0 then
      t[f.name] = read_value(f, base)
    end
  end
  return t
end

-- Write a Lua table into a named-field block.
function write_struct(fields, base, t)
  for i = 1, #fields do
    local f = fields[i]
    -- A MASKED FIELD IS WRITTEN AS EMPTY, NEVER SKIPPED. The buffer this lands
    -- in is reused across dispatches, so leaving the bytes alone would show the
    -- guest whatever the PREVIOUS event put there -- a stale presence byte
    -- reading 1 over a stale pointer, which is the silent-garbage class this
    -- whole layer exists to prevent. Zeroing a header is two stores against a
    -- deep copy, and the layout does not move: only a field the guest can read
    -- as ABSENT (an optional's presence byte) or as EMPTY (a container's
    -- ptr/count) is maskable at all, which is what mask_fields enforces.
    if f.skip then
      if f.has ~= nil then
        io_.st8(base + f.has, 0)
      else
        io_.st32(base + f.at, 0)
        io_.st32(base + f.at + 4, 0)
      end
    else
      local v = t and t[f.name]
      -- An absent optional is left alone: the presence byte already said so,
      -- and writing a default would cost time to say it twice. A mandatory
      -- field has no presence byte and is always written, because the guest
      -- reads it unconditionally.
      if f.has ~= nil then
        io_.st8(base + f.has, v ~= nil and 1 or 0)
      end
      if v ~= nil or f.has == nil then
        local st = write_value(f, base, v)
        if st ~= M.OK then return st end
      end
    end
  end
  return M.OK
end

-- Build the field list a MASKED subscription encodes with, from a bitmask over
-- field index -- bit 0 is fields[1], the order LayoutStruct placed them in.
--
-- Returns the new list and the names of the fields it REFUSED to mask. A
-- refusal is not an error: encoding a field the guest asked to skip is the
-- widening direction -- the same one an unreadable filter takes -- and it costs
-- time rather than correctness. The reverse, honouring a mask over a mandatory
-- scalar, would hand the guest a zero it cannot tell from a real one, which is
-- the silent-wrong-value class this ABI refuses everywhere else.
--
-- So exactly two shapes are maskable, and each has a reading every generated
-- decoder ALREADY produces:
--   * an OPTIONAL field -- presence byte 0, which the decoder reports as absent;
--   * a CONTAINER, array or dictionary -- (ptr, count) = (0, 0), which
--     read_array and the generated decoders turn into an empty collection.
-- Lying about the mask therefore yields EMPTY, never garbage.
function M.mask_fields(fields, bits)
  local out, refused = {}, {}
  for i = 1, #fields do
    local f = fields[i]
    -- Arithmetic rather than bit32: this file is Lua 5.2 in the Factorio
    -- sandbox, and the mask is small by construction (the widest event payload
    -- in 2.0.77 has 13 fields).
    local want = bits % (2 ^ i) >= 2 ^ (i - 1)
    if want and f.has == nil and f.kind ~= M.K_ARRAY and f.kind ~= M.K_DICT then
      refused[#refused + 1] = f.name
      want = false
    end
    if want then
      -- A SHALLOW COPY, because the generated table is shared: two
      -- subscriptions to one event must not see each other's mask, and the
      -- table is a module-level constant the next mod load reads again.
      local c = {}
      for k, v in pairs(f) do c[k] = v end
      c.skip = true
      out[i] = c
    else
      out[i] = f
    end
  end
  return out, refused
end

-- Read one described value at base + f.at. `f` carries its own offset so a
-- container can pass the container's base and let the field place itself.
function read_value(f, base)
  local k = f.kind
  if k == M.K_STRUCT then return read_struct(f.fields, base + f.at) end
  if k == M.K_ARRAY or k == M.K_DICT then return read_array(f, base + f.at) end
  if k == M.K_DYN then return read_dyn(base + f.at) end
  return read_field(k, base + f.at)
end

function write_value(f, base, v)
  local k = f.kind
  if k == M.K_STRUCT then return write_struct(f.fields, base + f.at, v) end
  if k == M.K_ARRAY or k == M.K_DICT then return write_array(f, base + f.at, v) end
  if k == M.K_DYN then return write_dyn(base + f.at, v) end
  return write_field(k, base + f.at, v)
end

-- An array is (ptr, count) with the elements out of line, for the same reason a
-- string is: how many there are is not known until the value exists.
--
-- A DICTIONARY IS THE SAME LAYOUT over key/value pairs, and only the table
-- built at the end differs. Sharing the walk is not just less code -- it means
-- a dict of structs, or an array of dicts, works without anyone having written
-- that case.
function read_array(f, at)
  local ptr, n = io_.ld32(at), io_.ld32(at + 4)
  local out = {}
  if ptr == 0 or n == 0 then return out end
  if f.kind == M.K_DICT then
    for i = 0, n - 1 do
      local e = ptr + i * f.stride
      out[read_value(f.key, e)] = read_value(f.elem, e)
    end
  else
    for i = 0, n - 1 do
      out[i + 1] = read_value(f.elem, ptr + i * f.stride)   -- Lua arrays are 1-based
    end
  end
  return out
end

function write_array(f, at, v)
  if v == nil then
    io_.st32(at, 0)
    io_.st32(at + 4, 0)
    return M.OK
  end
  -- Count first: the allocation size depends on it, and a dictionary has no #.
  local n, keys = 0, nil
  if f.kind == M.K_DICT then
    keys = {}
    for k in pairs(v) do n = n + 1 keys[n] = k end
    -- pairs() order is insertion order in this Lua and stable, but a guest
    -- reading a dictionary should not depend on it; nothing here promises one.
  else
    n = #v
  end
  if n == 0 then
    io_.st32(at, 0)
    io_.st32(at + 4, 0)
    return M.OK
  end
  if alloc_ == nil then return M.ERR_BAD_ARGS end
  local ptr = alloc_(n * f.stride)
  if ptr == nil or ptr == 0 then return M.ERR_NO_SPACE end
  for i = 0, n - 1 do
    local e = ptr + i * f.stride
    local st
    if keys then
      st = write_value(f.key, e, keys[i + 1])
      if st == M.OK then st = write_value(f.elem, e, v[keys[i + 1]]) end
    else
      st = write_value(f.elem, e, v[i + 1])
    end
    if st ~= M.OK then
      if free_ then free_(ptr) end
      return st
    end
  end
  io_.st32(at, ptr)
  io_.st32(at + 4, n)
  return M.OK
end

-- Read one field. Returns the Lua value.
function read_field(k, at)
  if k == M.K_I32 then return fold(io_.ld32(at), 2147483648.0, 4294967296.0) end
  if k == M.K_U32 then return io_.ld32(at) end
  if k == M.K_F64 then return io_.ldf64(at) end
  if k == M.K_F32 then return io_.ldf32(at) end
  if k == M.K_BOOL then return io_.ld8(at) ~= 0 end
  if k == M.K_STR then
    local ptr, n = io_.ld32(at), io_.ld32(at + 4)
    if n == 0 then return "" end
    return M.read_string(ptr, n)
  end
  if k == M.K_HANDLE then
    local obj = M.get(io_.ld32(at))
    return obj                        -- nil for a bad handle; the caller checks
  end
  if k == M.K_U8 then return io_.ld8(at) end
  if k == M.K_I8 then return fold(io_.ld8(at), 128.0, 256.0) end
  if k == M.K_U16 then return io_.ld16(at) end
  if k == M.K_I16 then return fold(io_.ld16(at), 32768.0, 65536.0) end
  if k == M.K_U64 then
    -- A double holds integers exactly only to 2^53, so a u64 above that cannot
    -- be represented at all -- the same limit string.pack has. Reassembled
    -- rather than refused, because every u64 the API actually returns (tick
    -- counts, ids) is far below the limit.
    return io_.ld32(at) + io_.ld32(at + 4) * 4294967296.0
  end
  return nil
end

-- Write one field. Returns a STATUS, not a boolean: "no allocator was bound"
-- and "the guest is out of memory" are different problems for whoever reads
-- the log, and collapsing them costs nothing to keep apart.
function write_field(k, at, v)
  if k == M.K_I32 or k == M.K_U32 then
    io_.st32(at, (v or 0) % 4294967296.0)
  elseif k == M.K_F64 then
    io_.stf64(at, v or 0.0)
  elseif k == M.K_F32 then
    io_.stf32(at, v or 0.0)
  elseif k == M.K_BOOL then
    io_.st8(at, v and 1 or 0)
  elseif k == M.K_HANDLE then
    -- Anything the host hands back is TRANSIENT by default. A guest that wants
    -- to keep it says so with fk_retain; one that forgets leaks nothing.
    io_.st32(at, M.transient(v))
  elseif k == M.K_U8 or k == M.K_I8 then
    io_.st8(at, (v or 0) % 256.0)
  elseif k == M.K_U16 or k == M.K_I16 then
    io_.st16(at, (v or 0) % 65536.0)
  elseif k == M.K_U64 then
    local x = v or 0
    local lo = x % 4294967296.0
    io_.st32(at, lo)
    io_.st32(at + 4, (x - lo) / 4294967296.0)
  elseif k == M.K_STR then
    local str = v
    if str == nil then
      io_.st32(at, 0)
      io_.st32(at + 4, 0)
      return M.OK, 0
    end
    if type(str) ~= "string" then str = tostring(str) end
    local n = #str
    if n == 0 then
      io_.st32(at, 0)
      io_.st32(at + 4, 0)
      return M.OK, 0
    end
    -- The scratch region first, falling back to the allocator when the string
    -- does not fit what is left. The fallback is what keeps a long string
    -- correct rather than truncated, and it is the reason the region can be
    -- small: it is sized for the common case, not the worst one.
    local p
    if scratchSize > 0 and scratchTop + n <= scratchSize then
      p = scratchBase + scratchTop
      scratchTop = scratchTop + n
    else
      if alloc_ == nil then return M.ERR_BAD_ARGS end
      p = alloc_(n)
      if p == nil or p == 0 then return M.ERR_NO_SPACE end
    end
    io_.wstr(p, str)
    io_.st32(at, p)
    io_.st32(at + 4, n)
    -- The second return is what a failed encode frees, so it must name ONLY a
    -- pointer the allocator owns. Handing back a scratch offset would have
    -- fk_free scan its pin list for an address that was never pinned -- today
    -- that is a harmless miss, but it is a miss by luck rather than by design.
    if scratchSize > 0 and p >= scratchBase and p < scratchBase + scratchSize then
      return M.OK, 0
    end
    return M.OK, p
  end
  return M.OK, 0
end

-- Write a variadic argument list as one tier-2 ARRAY.
--
-- It exists because `{...}` is not good enough and the difference is silent. A
-- table built that way has `#` stop at the first nil, so a remote interface
-- method called as `f(1, nil, 3)` would cross as a one-element array and the
-- guest would see the third argument vanish -- while `select("#", ...)` reports
-- the 3 the caller actually wrote. Every argument list a trampoline forwards
-- comes from another mod, so "nobody passes nil in the middle" is a hope about
-- somebody else's code rather than a property of this one.
--
-- The count is taken from the caller rather than from the table, and each slot
-- is written by index, so a hole crosses as DYN_NIL and the arity is preserved
-- exactly.
function M.write_varargs(at, ...)
  local n = select("#", ...)
  local ptr = 0
  if n > 0 then
    ptr = dyn_alloc(n * DYNW)
    if ptr == 0 then return M.ERR_NO_SPACE end
    for i = 1, n do
      local st = write_dyn(ptr + (i - 1) * DYNW, (select(i, ...)))
      if st ~= M.OK then
        dyn_free(ptr)
        return st
      end
    end
  end
  st32_(at, DYN_ARR)
  st32_(at + 8, ptr)
  st32_(at + 12, n)
  return M.OK
end

-- Exposed after both are defined. They are locals so the recursion between them
-- costs an upvalue read rather than a table index on every nested field; these
-- aliases are for the event encoder and for tests.
M.read_struct, M.write_struct = read_struct, write_struct
M.read_value, M.write_value = read_value, write_value
M.read_dyn, M.write_dyn = read_dyn, write_dyn

-- Decode an argument block into a Lua argument list.
--
-- `sig.args[i] = { kind = , at = }`, offsets precomputed by the generator so
-- nothing is recalculated per call.
function M.decode_args(sig, argp)
  local n = #sig.args
  if n == 0 then return true end
  local a = {}
  for i = 1, n do
    local f = sig.args[i]
    -- THE PRESENCE BYTE IS CONSULTED HERE TOO. read_struct honoured it for a
    -- nested field and this loop did not, so every optional ARGUMENT arrived
    -- present-and-zero: an absent boolean as false, an absent number as 0, an
    -- absent string as "". That is the exact distinction this layer says it
    -- keeps -- absent means leave it alone, present-false means turn it off --
    -- and `entity.teleport(pos, surface, raise_teleported)` is the shape it
    -- bites: a guest saying nothing was heard as saying NO.
    --
    -- unpack(a, 1, n) below carries the hole, so a nil in the middle stays in
    -- the middle rather than shortening the list.
    if f.has ~= nil and io_.ld8(argp + f.has) == 0 then
      a[i] = nil
    else
      a[i] = read_value(f, argp)
    end
  end
  return true, unpack(a, 1, n)
end

-- Encode results into the return block.
--
-- Allocations made along the way are tracked so a LATER field failing can undo
-- them. Without that, a two-string return whose second allocation fails leaves
-- the first block owned by nobody: the host has forgotten it and the guest
-- never saw the pointer, so it leaks for the life of the session.
function M.encode_rets(sig, retp, ...)
  local n = #sig.rets
  if n == 0 then return M.OK end
  local owned
  for i = 1, n do
    local f = sig.rets[i]
    local v = (select(i, ...))
    local st, p = M.OK, nil
    -- The presence byte, which the guest reads BEFORE the value. Omitting it
    -- left every optional return looking absent: the guest zeroes its return
    -- block, so a present value read as nil rather than as a wrong number --
    -- silent, and wrong in the direction that looks like the API said no.
    if f.has ~= nil then
      io_.st8(retp + f.has, v ~= nil and 1 or 0)
    end
    if v == nil and f.has ~= nil then
      -- Absent. The presence byte already said so and the value slot is the
      -- guest's own zeroed memory, so there is nothing to write.
    elseif f.kind == M.K_STRUCT or f.kind == M.K_ARRAY or f.kind == M.K_DICT or
       f.kind == M.K_DYN then
      st = write_value(f, retp, v)
    else
      st, p = write_field(f.kind, retp + f.at, v)
    end
    if st ~= M.OK then
      if owned and free_ then
        for j = 1, #owned do free_(owned[j]) end
      end
      return st
    end
    if p and p ~= 0 then
      owned = owned or {}
      owned[#owned + 1] = p
    end
  end
  return M.OK
end

-- The EQ kind's whole body: read the attribute, compare it against a string
-- still sitting in guest memory, write a bool.
--
-- IT DOES NOT GO THROUGH decode_args, AND THAT IS THE POINT. Decoding a string
-- argument means walking the guest's word table and building a Lua string --
-- 14 ns a byte, measured -- which for the shape this exists for is work thrown
-- away: a guest asking `entity.name == "transport-belt"` on every build event
-- is asking a question whose answer is usually NO, and a length that does not
-- match settles it without reading a byte.
--
-- Measured through the real guest, --persist=table: with the decode, the
-- predicate cost the same as reading the name and comparing it in the guest
-- (4,484 ns against 4,269) and won only on heap. The direction is why -- the
-- host WRITING a string into the guest is 6.44 ns/byte and READING one back out
-- is 14 -- so a predicate that always decodes trades a cheap write for an
-- expensive read. The length check is what makes the common answer cheap.
--
-- Everything else is M.invoke's: the handle resolution, the `valid` check, the
-- pcall around the member read, ERR_NO_MEMBER. This function adds a comparison
-- and a byte.
local function call_eq(m, h, mid, argp, retp)
  local st, f = M.invoke(h, mid)
  if st ~= M.OK then return st end
  -- The argument's own offset, from the layout, rather than 0: a string is
  -- (ptr, len) wherever the generator placed it, and hardcoding the place is
  -- how a layout change becomes a wrong answer instead of a build error.
  local at = argp + m.sig.args[1].at
  local n = io_.ld32(at + 4)
  local eq = type(f) == "string" and #f == n
  if eq and n > 0 then
    eq = f == M.read_string(io_.ld32(at), n)
  end
  -- `type(f) == "string"` rather than a tostring: the generator emits this kind
  -- only for an attribute the API declares as a plain string, so anything else
  -- means the running Factorio disagrees with the description this mod was
  -- built against -- and "no, it is not equal to that string" is the honest
  -- answer, not a coercion.
  io_.st8(retp + m.sig.rets[1].at, eq and 1 or 0)
  return M.OK
end

-- The import the guest calls. Everything above exists to make this one line
-- long, and to make each half testable without the other.
function M.call(h, mid, argp, retp)
  -- THE MESSAGE SLOT IS CLEARED HERE AND NOWHERE ELSE, which is what lets
  -- M.last_error mean "the call that just returned" rather than "whatever
  -- failed last, ever". One upvalue store per host call; see M.last_error for
  -- the whole contract and for the one re-entrant seam it leaves.
  lastError = ""
  local m = members[mid]
  if m == nil or m.sig == nil then return M.ERR_NO_MEMBER end
  if m.kind == M.EQ then return call_eq(m, h, mid, argp, retp) end
  -- Where the scratch region stood when this call began. Everything below is
  -- somebody else's and stays untouched; everything at or above it is ours to
  -- reuse once the things that wrote it are finished.
  local mark = M.scratch_mark()
  local ok, r1, r2, r3, r4 = M.decode_args(m.sig, argp)
  if not ok then return M.ERR_BAD_ARGS end
  -- FORWARD EXACTLY THE DECLARED ARITY. Lua counts trailing nils, and the
  -- Factorio engine's argument checker is strict, so handing four slots to a
  -- one-argument method is "Expected 1 argument but 4 were given" -- an
  -- ERR_CALL_FAILED on every member with fewer than four arguments, which is
  -- almost all of them. A dispatch chain rather than `unpack` on a per-call
  -- table: this is the hot path in a lockstep game loop and four branches cost
  -- less than an allocation.
  --
  -- AND TRIM IT TO THE LAST ARGUMENT ACTUALLY PRESENT, which is the other half
  -- and was missing until this landed. The declared count is the upper bound,
  -- not the number to send: decode_args honours presence bytes, so an absent
  -- trailing optional becomes a real nil -- and a real nil is an argument the
  -- engine COUNTED and then type-checked:
  --
  --   bad argument #2 of 3 to '?' (table expected, got nil)
  --
  -- on game.create_surface(name), and on every member in the API with a
  -- trailing optional number or boolean, which a guest could then not call at
  -- all. The declared-arity rule and the absent-optional rule were each correct
  -- on their own; they meet only at a member with a trailing optional called
  -- against a host that counts, which is the whole Factorio API and nothing
  -- this suite had.
  --
  -- A hole in the MIDDLE still crosses as nil, because Lua has no other way to
  -- say it. The trim stops at the last present argument rather than removing
  -- every absent one.
  local n = #m.sig.args
  while n > 0 do
    local f = m.sig.args[n]
    if f.has == nil or io_.ld8(argp + f.has) ~= 0 then break end
    n = n - 1
  end
  local st, a, b, c, d
  if n == 0 then
    st, a, b, c, d = M.invoke(h, mid)
  elseif n == 1 then
    st, a, b, c, d = M.invoke(h, mid, r1)
  elseif n == 2 then
    st, a, b, c, d = M.invoke(h, mid, r1, r2)
  elseif n == 3 then
    st, a, b, c, d = M.invoke(h, mid, r1, r2, r3)
  else
    st, a, b, c, d = M.invoke(h, mid, r1, r2, r3, r4)
  end
  if st ~= M.OK then return st end
  -- invoke may have raised an event synchronously, whose handler made host
  -- calls of its own. All of that is finished now and its strings have been
  -- copied out, so the region goes back to our mark before we write over it.
  -- NOT to zero: an event further out may still be being read.
  M.scratch_release(mark)
  return M.encode_rets(m.sig, retp, a, b, c, d)
end

-- read_string is supplied alongside memio, since it needs the same MEM.
function M.bind_read_string(f) M.read_string = f end

-- Counts, for tests and for a diagnostic a mod author can log.
function M.stats()
  local np, nt = 0, 0
  for _ in pairs(persistent) do np = np + 1 end
  for _ in pairs(transient) do nt = nt + 1 end
  return np, nt, #free
end

return M
