package factorio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Techrocket9/fklua/internal/luahost"
	luart "github.com/Techrocket9/fklua/runtime"
)

// runABI drives runtime/lua/fk_abi.lua under the sandbox-shaped interpreter.
//
// The ABI is hand-written Lua that never passes through the compiler, so
// nothing else in the suite would exercise it. It is also the piece a guest's
// every interaction with the game goes through, which makes "untested" the
// wrong state for it to be in.
func runABI(t *testing.T, script string) string {
	t.Helper()
	h, err := luahost.Find()
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fk_abi.lua"), []byte(luart.ABI()), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package.path = " + luaQuote(filepath.Join(dir, "?.lua")) + "\n" +
		"local H = require(\"fk_abi\")\n" + script
	out, err := h.RunString(src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return strings.TrimSpace(out)
}

// The property the transient space exists for: a handler that takes a handle
// and drops it leaks nothing, with no discipline required of the guest author.
//
// This is the dominant leak shape -- read event.entity, use it, return -- and
// under a single-space design every one of them would pin an entry forever.
func TestTransientHandlesAreReleasedWholesale(t *testing.T) {
	got := runABI(t, `
local ent = { valid = true }
H.bind_globals({})
for i = 1, 1000 do H.transient(ent) end
local _, nt = H.stats()
H.clear_transient()
local np, nt2 = H.stats()
print(nt .. " " .. nt2 .. " " .. np)
`)
	if got != "1000 0 0" {
		t.Errorf("got %q, want %q (allocated, after clear, leaked to persistent)", got, "1000 0 0")
	}
}

// retain is the opt-in that survives the clear, and release returns the slot.
func TestRetainAndRelease(t *testing.T) {
	got := runABI(t, `
local ent = { valid = true }
H.bind_globals({})
local p = H.retain(H.transient(ent))
H.clear_transient()
print((H.get(p)) == ent and "survived" or "LOST")
print(H.release(p) == H.OK and "released" or "FAILED")
print(select(2, H.get(p)) == H.ERR_BAD_HANDLE and "gone" or "STILL THERE")
print(H.release(p) == H.ERR_BAD_HANDLE and "double-release refused" or "DOUBLE OK")
-- The freed slot is reused rather than the space growing forever.
print(H.retain(H.transient(ent)) == p and "slot reused" or "SLOT LEAKED")
`)
	want := "survived\nreleased\ngone\ndouble-release refused\nslot reused"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An invalidated LuaObject must come back as a STATUS. Factorio raises a Lua
// error when one is touched, and that error would unwind through wasm frames
// that cannot be unwound -- there are no coroutines to resume.
func TestAnInvalidatedObjectIsAStatusNotACrash(t *testing.T) {
	got := runABI(t, `
H.bind_globals({})
H.bind_members({ [1] = { kind = H.GET, name = "name", valid = true } })
local h = H.transient({ valid = false, name = "x" })
local st = H.invoke(h, 1)
print("nil " .. (st == H.ERR_INVALID and "ERR_INVALID" or ("code " .. st)))
-- And a class with no valid attribute is never probed. Reading a key a
-- LuaObject does not have RAISES rather than returning nil, so a blanket probe
-- crashed every call on the game object until real Factorio said so.
H.bind_members({ [1] = { kind = H.GET, name = "speed" } })
local raises = setmetatable({}, { __index = function(_, k)
  if k == "valid" then error("LuaGameScript doesn't contain key valid") end
  return 42
end })
local st2, v = H.invoke(H.transient(raises), 1)
print("unprobed " .. st2 .. " " .. tostring(v))
`)
	want := "nil ERR_INVALID\nunprobed 0 42"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The 1..9 block is part of the ABI: a guest compiled against it uses the
// numbers directly, so the ORDER is a compatibility surface. Appending is safe,
// reordering is not, and this pins it.
func TestGlobalHandleOrderIsFixed(t *testing.T) {
	got := runABI(t, `
print(table.concat(H.GLOBAL_NAMES, ","))
H.bind_globals({ commands = "c", game = "g", helpers = "h", prototypes = "p",
                 rcon = "rc", remote = "rm", rendering = "rd", script = "s",
                 settings = "st" })
local out = {}
for i = 1, 9 do out[i] = tostring((H.get(i))) end
print(table.concat(out, ","))
`)
	want := "commands,game,helpers,prototypes,rcon,remote,rendering,script,settings\n" +
		"c,g,h,p,rc,rm,rd,s,st"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A global the sandbox does not expose must be a bad handle rather than a nil
// that something later indexes.
func TestAnAbsentGlobalIsABadHandle(t *testing.T) {
	got := runABI(t, `
H.bind_globals({ game = "g" })
print(select(2, H.get(1)) == H.ERR_BAD_HANDLE and "absent is refused" or "LEAKED NIL")
print((H.get(2)) == "g" and "present resolves" or "MISSING")
`)
	if want := "absent is refused\npresent resolves"; got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Across a save the persistent table crosses and the free list is REBUILT.
// Storing the free list instead would hand out a slot that is still in use --
// two guest handles aliasing one object, which is corruption rather than a leak.
func TestAdoptRebuildsTheFreeList(t *testing.T) {
	got := runABI(t, `
H.bind_globals({})
-- A save where slot 11 had been released before saving.
H.adopt({ [10] = "A", [12] = "C" })
local np, _, nfree = H.stats()
print(np .. " live, " .. nfree .. " free")
print((H.get(10)) .. (H.get(12)))
local reused = H.retain(H.transient({ valid = true }))
print("next slot " .. reused)
-- and the one after it extends the space rather than colliding
print("then " .. H.retain(H.transient({ valid = true })))
`)
	want := "2 live, 1 free\nAC\nnext slot 11\nthen 13"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The persistent table is the live one, so a retain during play lands directly
// in what Factorio serializes -- the same aliasing --persist=table uses for
// guest memory, and the reason there is no sync step.
func TestThePersistentTableIsLive(t *testing.T) {
	got := runABI(t, `
H.bind_globals({})
local saved = H.persistent_table()
local ent = { valid = true }
local p = H.retain(H.transient(ent))
print(saved[p] == ent and "aliased" or "COPIED")
`)
	if got != "aliased" {
		t.Errorf("got %q, want %q", got, "aliased")
	}
}

// Dispatch, end to end against a stubbed game object. The three member kinds
// are generic over every class -- reading obj[name] and calling obj[name](...)
// needs no per-class code -- so this is the whole mechanism.
func TestInvokeCoversTheThreeMemberKinds(t *testing.T) {
	got := runABI(t, `
local ent = { valid = true, name = "iron-chest", health = 100 }
-- A method is a CLOSURE OVER THE OBJECT, which is what Factorio's __index hands
-- back; it takes no self, and a stub that declares one is the shape that hid
-- D1 for as long as this suite has existed.
ent.damage = function(amount) ent.health = ent.health - amount return ent.health, "ok" end
H.bind_globals({})
H.bind_members({
  [1] = { kind = H.GET,  name = "name" },
  [2] = { kind = H.SET,  name = "health" },
  [3] = { kind = H.CALL, name = "damage" },
})
local h = H.transient(ent)
print((select(2, H.invoke(h, 1))))
print(H.invoke(h, 2, 250) == H.OK and ent.health == 250 and "set" or "SET FAILED")
local st, left, word = H.invoke(h, 3, 40)
print(st .. " " .. left .. " " .. word)
`)
	want := "iron-chest\nset\n0 210 ok"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A member this Factorio does not have must NOT take the mod down. It returns a
// status, and it is reported ONCE -- a guest calling a removed member every
// tick would otherwise write sixty log lines a second and bury the one that
// mattered.
func TestAMissingMemberIsReportedOnceAndReturnsAStatus(t *testing.T) {
	got := runABI(t, `
local lines = 0
log = function() lines = lines + 1 end
H.bind_globals({})
H.bind_members({ [1] = { kind = H.CALL, name = "method_from_the_future" } })
local h = H.transient({ valid = true })
local first = H.invoke(h, 1)
for i = 1, 100 do H.invoke(h, 1) end
print((first == H.ERR_NO_MEMBER) and "ERR_NO_MEMBER" or ("code " .. first))
print("logged " .. lines .. " of 101 calls")
`)
	want := "ERR_NO_MEMBER\nlogged 1 of 101 calls"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An error raised by the Factorio API must become a status, never an unwind.
// There are no coroutines, so an error crossing back into wasm cannot unwind
// the frame it came from -- it would take down the whole mod instead of the one
// call the guest could have handled.
func TestAnAPIErrorBecomesAStatus(t *testing.T) {
	got := runABI(t, `
H.bind_globals({})
H.bind_members({
  [1] = { kind = H.CALL, name = "explode" },
  [2] = { kind = H.GET,  name = "cursed" },
})
local obj = setmetatable({
  valid = true,
  explode = function() error("the API said no") end,
}, { __index = function(_, k)
  if k == "cursed" then error("reading this raises") end
end })
local h = H.transient(obj)
print(H.invoke(h, 1) == H.ERR_CALL_FAILED and "call trapped" or "LEAKED")
print(H.last_error():match("the API said no") and "message kept" or "MESSAGE LOST")
print(H.invoke(h, 2) == H.ERR_CALL_FAILED and "read trapped" or "READ LEAKED")
`)
	want := "call trapped\nmessage kept\nread trapped"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Dispatch goes through the handle table, so every handle failure is a dispatch
// failure with the same code -- a guest sees one error vocabulary, not two.
func TestInvokeInheritsHandleStatuses(t *testing.T) {
	got := runABI(t, `
H.bind_globals({})
-- valid=true because this member's class has the attribute; the generator sets
-- it from the API description, and without it the check is skipped by design.
H.bind_members({ [1] = { kind = H.GET, name = "name", valid = true } })
print(H.invoke(0, 1) == H.ERR_BAD_HANDLE and "bad handle" or "WRONG")
print(H.invoke(H.transient({ valid = false }), 1) == H.ERR_INVALID and "invalid" or "WRONG")
print(H.invoke(H.transient({ valid = true }), 99) == H.ERR_NO_MEMBER and "no member id" or "WRONG")
`)
	want := "bad handle\ninvalid\nno member id"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// An optional return that HAS a value must say so.
//
// encode_rets wrote the value and never the presence byte, so every optional
// return arrived looking absent. That is the worst shape a bug can have here:
// the guest zeroes its own return block, so a present value read as nil rather
// than as a wrong number -- indistinguishable from the API legitimately saying
// "no value", and wrong in the direction a caller is least likely to question.
//
// It survived because nothing exercised the case. The per-class dispatch gate
// skips optional returns by construction, and the marshalling tests all used
// mandatory ones.
func TestAnOptionalReturnWritesItsPresenceByte(t *testing.T) {
	got := runABI(t, `
H.bind_globals({})
-- One member, one optional u32 return: presence at 0, value at 4.
H.bind_members({ [1] = { kind = H.CALL, name = "maybe", argsize = 0, retsize = 8,
  sig = { args = {}, rets = { { name = "r0", kind = H.K_U32, at = 4, has = 0 } } } } })

local mem = {}
H.bind_memory({
  ld8 = function(a) return mem[a] or 0 end,
  st8 = function(a, v) mem[a] = v end,
  ld32 = function(a) return mem[a] or 0 end,
  st32 = function(a, v) mem[a] = v end,
})

local obj = { valid = true, maybe = function() return 42 end }
print("present " .. H.call(H.transient(obj), 1, 0, 100) ..
      " has=" .. (mem[100] or 0) .. " value=" .. (mem[104] or 0))

-- And absent still reads absent, so the fix did not just hardwire a 1.
mem[100] = nil
local none = { valid = true, maybe = function() return nil end }
print("absent " .. H.call(H.transient(none), 1, 0, 100) ..
      " has=" .. (mem[100] or 0))
`)
	want := "present 0 has=1 value=42\nabsent 0 has=0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A Factorio LuaObject is not a table with functions in it. Its `__index`
// returns a closure ALREADY BOUND to the object -- which is why every line of
// real mod code is `surface.create_entity{...}`, dot-called, never
// colon-called -- and the engine's argument checker counts what arrives
// exactly. Two things follow, and this layer got both wrong:
//
//  1. `pcall(f, obj, ...)` passes the object a second time.
//  2. M.call forwarded all four decoded slots regardless of the member's
//     declared arity, and Lua counts trailing nils.
//
// So `game.get_surface("nauvis")` arrived as five arguments and came back
// ERR_CALL_FAILED. Nothing in this suite could see it: every other test drives
// M.invoke directly with the right number of arguments -- so the padding line
// is never on the path -- and every stub declares its methods as
// `function(self, x)` left in the table, which is exactly the shape that makes
// the spurious `obj` look correct.
func TestAMethodIsCalledTheWayFactorioBindsIt(t *testing.T) {
	got := runABI(t, `
-- A stand-in built the way the ENGINE builds one, not the way a test would.
local function luaobject(fields, methods)
  local o
  o = setmetatable({}, {
    __index = function(_, k)
      local m = methods[k]
      if m == nil then return fields[k] end
      return function(...)
        local n = select("#", ...)
        if n ~= m.arity then
          error("Arguments count error for '" .. k .. "': Expected " ..
                m.arity .. " argument but " .. n .. " were given")
        end
        return m.fn(o, ...)
      end
    end,
    __newindex = function(_, k, v) fields[k] = v end,
  })
  return o
end

H.bind_globals({})
local mem = {}
H.bind_memory({
  ld8  = function(a) return mem[a] or 0 end,
  st8  = function(a, v) mem[a] = v end,
  ld32 = function(a) return mem[a] or 0 end,
  st32 = function(a, v) mem[a] = v end,
})

local u32 = function(at) return { kind = H.K_U32, at = at } end
H.bind_members({
  -- nullary: the shape every no-argument method has, and the one the padding
  -- broke worst -- four nils where the engine expects none.
  [1] = { kind = H.CALL, name = "clear",
          sig = { args = {}, rets = { u32(0) } } },
  [2] = { kind = H.CALL, name = "get_surface",
          sig = { args = { u32(0) }, rets = { u32(0) } } },
  [3] = { kind = H.CALL, name = "scale",
          sig = { args = { u32(0), u32(4) }, rets = { u32(0) } } },
})

local obj = luaobject({ valid = true }, {
  clear       = { arity = 0, fn = function() return 7 end },
  get_surface = { arity = 1, fn = function(_, a) return a + 1 end },
  scale       = { arity = 2, fn = function(_, a, b) return a * b end },
})
local h = H.transient(obj)

local argp, retp = 64, 128
local function one(mid, a, b)
  mem[argp], mem[argp + 4] = a, b
  mem[retp] = 0
  local st = H.call(h, mid, argp, retp)
  if st ~= H.OK then return "status " .. st .. ": " .. H.last_error() end
  return tostring(mem[retp])
end
print(one(1))
print(one(2, 41))
print(one(3, 6, 7))
`)
	want := "7\n42\n42"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// AN ABSENT OPTIONAL ARGUMENT MUST CROSS AS NIL, NOT AS ITS ZERO VALUE.
//
// read_struct consults a field's presence byte; decode_args, which is what reads
// the TOP-LEVEL argument list, never did. So every optional argument of every
// method arrived present-and-zero: an absent boolean as false, an absent number
// as 0, an absent string as "". That is the exact distinction this ABI says it
// keeps -- "an absent optional is omitted, not defaulted, because Factorio
// distinguishes absent from present-and-false throughout: absent means leave it
// alone, present-false means turn it off" -- and it was true of returns and of
// struct fields but not of arguments.
//
// entity.teleport(position, surface, raise_teleported) is the shape that bites:
// a guest passing nil for raise_teleported was telling the game NO rather than
// saying nothing, and the difference is whether other mods see the event.
//
// The arity half of this used to be asserted here and asserted WRONG -- that
// forwarding #sig.args values with a trailing nil was inside the engine's
// range. It is not: the engine counts an explicit nil as an argument given and
// then type-checks it. That is item 16 downstream, and it lives in
// TestATrailingAbsentOptionalIsNotForwardedAtAll; what this test keeps is the
// distinction the decode owes -- absent is nil, present-and-zero is zero.
func TestAnAbsentOptionalArgumentCrossesAsNil(t *testing.T) {
	got := runABI(t, `
local seen
H.bind_globals({})
local mem = {}
H.bind_memory({
  ld8  = function(a) return mem[a] or 0 end,
  st8  = function(a, v) mem[a] = v end,
  ld32 = function(a) return mem[a] or 0 end,
  st32 = function(a, v) mem[a] = v end,
})
H.bind_read_string(function(p, n) return "nauvis" end)

-- The engine's own shape: a bound closure, and an argument count checked
-- against the declared MIN and MAX rather than a single number.
local obj = setmetatable({}, { __index = function(_, k)
  if k ~= "create_surface" then return nil end
  return function(...)
    local n = select("#", ...)
    if n < 1 or n > 2 then
      error("Arguments count error for 'create_surface': Expected from 1 to 2 " ..
            "arguments but " .. n .. " were given")
    end
    seen = n .. " " .. tostring((select(1, ...))) .. " " .. tostring((select(2, ...)))
    return { valid = true }
  end
end })

H.bind_members({
  [1] = { kind = H.CALL, name = "create_surface", sig = {
    args = { { kind = H.K_STR, at = 0 },
             { kind = H.K_U32, at = 8, has = 12 } },
    rets = { { kind = H.K_HANDLE, at = 0 } } } },
})
-- The name is present; the second argument's presence byte is not set, which is
-- all the guest does to leave an optional out.
mem[64], mem[68] = 512, 6
print("status " .. H.call(H.transient(obj), 1, 64, 128))
print(seen)
-- ...and present-and-zero still arrives as zero, so the fix did not turn every
-- falsy value into an absent one.
mem[76] = 1
print("status " .. H.call(H.transient(obj), 1, 64, 128))
print(seen)
`)
	// One argument for the absent case: the trailing optional is trimmed rather
	// than sent as a nil the engine would reject.
	want := "status 0\n1 nauvis nil\nstatus 0\n2 nauvis 0"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A TRAILING OPTIONAL NOBODY PASSED MUST NOT BE FORWARDED AT ALL.
//
// The arity fix and the absent-optional fix are each right and were together
// wrong. M.call forwarded the DECLARED count, which is what killed the
// four-nil padding; decode_args then started honouring presence bytes, so an
// absent optional became a real nil. A member with a trailing optional
// therefore reached the engine as N arguments whose last was an explicit nil --
// and Factorio type-checks it:
//
//	bad argument #2 of 3 to '?' (table expected, got nil)
//
// game.create_surface(name) is exactly that shape, and every optional argument
// in the API is. Nothing here caught it because the arity tests use members
// whose arguments are all mandatory, and the absent-optional test asserted the
// nil arrived -- which was the wrong assertion, made against a stub whose
// checker only counted.
//
// The fix is to trim the forwarded arity to the last argument actually PRESENT.
// The declared count stays the upper bound, and a hole in the MIDDLE still
// crosses as nil, because there is nothing else it could be.
func TestATrailingAbsentOptionalIsNotForwardedAtAll(t *testing.T) {
	got := runABI(t, `
local seen
H.bind_globals({})
local mem = {}
H.bind_memory({
  ld8  = function(a) return mem[a] or 0 end,
  st8  = function(a, v) mem[a] = v end,
  ld32 = function(a) return mem[a] or 0 end,
  st32 = function(a, v) mem[a] = v end,
})
H.bind_read_string(function(p, n) return "nauvis" end)

-- The engine's shape, including the half that actually bit: it counts, AND it
-- type-checks what arrives. An explicit nil where a table is declared is an
-- error, not an omission.
local obj = setmetatable({}, { __index = function(_, k)
  return function(...)
    local n = select("#", ...)
    if n < 1 or n > 3 then
      error("Arguments count error for '" .. k .. "': Expected from 1 to 3 " ..
            "arguments but " .. n .. " were given")
    end
    -- A TRAILING explicit nil is what the engine rejects: the argument WAS
    -- given, so it gets type-checked, and nil is not a table. A hole in the
    -- middle is a different question, and Lua offers no other way to express
    -- one -- which is why the trim stops at the last present argument rather
    -- than removing every absent optional.
    if n == 2 and select(2, ...) == nil then
      error("bad argument #2 of 3 to '?' (table expected, got nil)")
    end
    seen = n .. " " .. tostring((select(1, ...))) .. " " ..
           tostring((select(2, ...))) .. " " .. tostring((select(3, ...)))
    return { valid = true }
  end
end })

-- create_surface(name, settings?) -- the shape the whole downstream mod rests
-- on. And a three-argument member whose MIDDLE argument is the optional one,
-- which is the case the trim must not swallow.
H.bind_members({
  [1] = { kind = H.CALL, name = "create_surface", sig = {
    args = { { kind = H.K_STR, at = 0 },
             { kind = H.K_U32, at = 8, has = 12 } },
    rets = { { kind = H.K_HANDLE, at = 0 } } } },
  [2] = { kind = H.CALL, name = "middle", sig = {
    args = { { kind = H.K_STR, at = 0 },
             { kind = H.K_U32, at = 8, has = 12 },
             { kind = H.K_U32, at = 16 } },
    rets = { { kind = H.K_HANDLE, at = 0 } } } },
})

mem[64], mem[68] = 512, 6
print("status " .. H.call(H.transient(obj), 1, 64, 128) .. " " .. H.last_error())
print(seen)

-- Present again: the argument comes back, so the trim is about presence and not
-- about the value being falsy.
mem[76] = 1
print("status " .. H.call(H.transient(obj), 1, 64, 128))
print(seen)

-- A hole in the middle. Nothing is trimmed, because something after it is
-- present, and the nil is the only thing the middle slot can be.
mem[76], mem[80] = 0, 9
print("status " .. H.call(H.transient(obj), 2, 64, 128))
print(seen)
`)
	want := "status 0 \n1 nauvis nil nil\nstatus 0\n2 nauvis 0 nil\nstatus 0\n3 nauvis nil 9"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
