-- Asserts that lua52f actually has Factorio's shape.
--
-- This runs in CI on every push. If lua52f drifts from Factorio's sandbox, every
-- host-side test result becomes a lie -- a passing spec suite would tell us
-- nothing about whether the generated Lua runs in game. So the oracle gets
-- checked before it is trusted.
--
-- Reference: https://lua-api.factorio.com/latest/auxiliary/libraries.html

local fail = 0
local function check(name, ok, detail)
  if ok then
    print(string.format("  ok    %s", name))
  else
    fail = fail + 1
    print(string.format("  FAIL  %s%s", name, detail and ("  -- " .. detail) or ""))
  end
end
local function note(name, detail)
  print(string.format("  note  %s  -- %s", name, detail))
end

print("lua52f sandbox conformance (" .. _VERSION .. ")")

-- 1. Version -----------------------------------------------------------------
check("_VERSION is Lua 5.2", _VERSION == "Lua 5.2", tostring(_VERSION))

-- 2. Removed modules ---------------------------------------------------------
for _, n in ipairs({ "coroutine", "io", "os" }) do
  check("global '" .. n .. "' absent", rawget(_G, n) == nil)
end
for _, n in ipairs({ "loadfile", "dofile" }) do
  check("base function '" .. n .. "' absent", rawget(_G, n) == nil)
end

-- 2a. collectgarbage is PRESENT in Factorio, with every 5.2 option --------------
--
-- Verified in game (probe.json `g_collectgarbage`, `cg_*`), 2.0.77. It is here
-- because the oracle must not drift the other way either: a guest's linear
-- memory is one huge Lua array table, and any future claim about pacing the
-- collector around it has to be testable here first. It is NOT a lever -- Lua
-- traverses a table in one indivisible propagatemark, and setpause/setstepmul
-- were measured in game to move a 12.7 ms pause by less than its own noise.
-- See agents/guests.md, "the guest heap budget".
check("collectgarbage present", type(collectgarbage) == "function")
for _, opt in ipairs({ "count", "step", "collect", "isrunning",
                       "setpause", "setstepmul" }) do
  check("collectgarbage('" .. opt .. "') accepted",
    (pcall(collectgarbage, opt, 200)))
end
collectgarbage("setpause", 200)
collectgarbage("setstepmul", 200)

-- 3. load() accepts source, rejects bytecode ---------------------------------
local f = load("return 21 * 2")
check("load() accepts a text chunk", type(f) == "function" and f() == 42)

if string.dump then
  local bin = string.dump(function() return 1 end)
  check("bytecode really starts with the Lua signature", bin:sub(1, 4) == "\27Lua")
  local g, err = load(bin)
  check("load() rejects a binary chunk", g == nil, tostring(err))
  -- Factorio: "the mode argument has no effect"
  local h = load(bin, "chunk", "b")
  check("load() ignores mode='b'", h == nil)
else
  note("string.dump absent", "cannot construct a binary chunk to reject")
end

-- 4. Numbers are doubles, with no integer subtype -----------------------------
check("math.type absent (no integer subtype)", math.type == nil)
check("3/2 is not truncated", 3 / 2 == 1.5)
check("integers exact to 2^53", 2 ^ 53 == 2 ^ 53 + 0 and (2 ^ 53 + 1) == 2 ^ 53)
check("no integer-division operator", load("return 7//2") == nil)
check("no bitwise operators", load("return 1<<2") == nil)

-- 5. bit32 ------------------------------------------------------------------
check("bit32 present", type(bit32) == "table")
if type(bit32) == "table" then
  for _, n in ipairs({ "band", "bor", "bxor", "bnot", "lshift", "rshift",
                       "arshift", "lrotate", "rrotate", "extract", "replace" }) do
    check("bit32." .. n, type(bit32[n]) == "function")
  end
  check("bit32 returns unsigned", bit32.bnot(0) == 4294967295)
end

-- 6. Float helpers we depend on for bit-punning -------------------------------
check("math.frexp present", type(math.frexp) == "function")
check("math.ldexp present", type(math.ldexp) == "function")

-- string.pack is backported into Factorio from 5.4.6 and is NOT in stock 5.2.1;
-- patches/02-string-pack.patch grafts it in. ASSERTED, not reported: this sat
-- behind `if string.pack then` from M0 to M6, so the branch never ran and the
-- oracle silently lacked a feature the game has, while CI claimed otherwise.
--
-- The values are the ones the in-game probe measured against Factorio 2.0.77
-- (testdata/probe/results/probe.json). The two that matter most are the two
-- that differ from upstream 5.4, because they are what "doubles-only" means.
check("string.pack present", type(string.pack) == "function")
check("string.unpack present", type(string.unpack) == "function")
check("string.packsize present", type(string.packsize) == "function")
check("string.pack round-trips u32",
  string.unpack("<I4", string.pack("<I4", 4000000000)) == 4000000000)
-- Real 5.4 raises "number has no integer representation" here.
check("string.pack truncates a fractional double",
  string.unpack("<I4", string.pack("<I4", 3.5)) == 3)
-- A double cannot represent 2^53+1, so <I8 cannot carry an arbitrary i64.
check("string.pack <I8 saturates at 2^53",
  string.unpack("<I8", string.pack("<I8", 2 ^ 53 + 1)) == 9007199254740992)
check("string.pack <i8 round-trips a negative",
  string.unpack("<i8", string.pack("<i8", -1)) == -1)
check("string.packsize", string.packsize(string.rep("<I4", 1024)) == 4096)

-- 7. goto, and Invariant B (all locals declared before any label) --------------
check("goto with all-locals-first compiles",
  load("local a,b,c a=1 ::top:: b=2 if a<1 then goto top end c=3 return c") ~= nil)
check("jumping into a local's scope is rejected",
  load("goto skip local x = 1 ::skip:: return 0") == nil)

-- 7a. The ONE local the emitter declares after the prologue: a numeric `for`'s
-- control variable, from the counted-loop lowering.
--
-- Invariant B exists because Lua rejects a goto that jumps INTO a local's
-- scope. A `for` body is such a scope, so everything below is the evidence that
-- putting one inside flat, goto-based output does not break the goto-based
-- output around it. wasm's structured control flow cannot name a label inside a
-- loop body from outside it, so the rejected case is also unreachable -- but it
-- is checked here rather than argued, because the emitter's whole control-flow
-- model rests on it.
check("a goto OUT of a for body to a function-level label compiles",
  load("local s = 0 for i = 1, 3 do s = s + i if s > 2 then goto done end end ::done:: return s") ~= nil)
check("a backward goto ACROSS a for statement compiles",
  load("local n = 0 ::top:: for i = 1, 2 do n = n + 1 end if n < 4 then goto top end return n") ~= nil)
check("a label inside a for body, jumped to from inside it, compiles",
  load("local s = 0 for i = 1, 3 do if i == 2 then goto cont end s = s + i ::cont:: end return s") ~= nil)
check("jumping INTO a for body from outside is rejected",
  load("goto inner for i = 1, 3 do ::inner:: end return 0") == nil)
-- The counter's scope is the loop's, and the initial expression is evaluated in
-- the ENCLOSING scope -- which is what lets the emitter write `for v1 = v1, n`
-- and have it mean "start from whatever v1 already held".
check("a for variable takes its initial value from the outer name",
  (load("local v = 7 local seen = 0 for v = v, v + 2 do seen = seen + v end return seen"))() == 24)
check("a for variable does not outlive its loop",
  (load("local v = 7 for v = v, v + 2 do end return v"))() == 7)

-- 8. Lua 5.2 codegen limits our emitter must respect --------------------------
local function nlocals(n)
  local t = {}
  for i = 1, n do t[i] = "local v" .. i end
  return load(table.concat(t, " ") .. " return 0")
end
check("199 locals compile", nlocals(199) ~= nil)
check("200 locals compile", nlocals(200) ~= nil)
check("201 locals rejected", nlocals(201) == nil)

-- 9. debug is restricted to getinfo + traceback -------------------------------
check("debug.getinfo present", type(debug.getinfo) == "function")
check("debug.traceback present", type(debug.traceback) == "function")
for _, n in ipairs({ "sethook", "gethook", "getlocal", "setlocal",
                     "getupvalue", "setupvalue", "getregistry", "debug" }) do
  check("debug." .. n .. " absent", debug[n] == nil)
end

-- 10. Things the emitter relies on being present ------------------------------
for _, n in ipairs({ "pcall", "xpcall", "select", "rawget", "rawset", "error" }) do
  check("global '" .. n .. "'", type(_G[n]) == "function")
end
check("table.concat", type(table.concat) == "function")
check("table.unpack", type(table.unpack) == "function")
check("hex float literals parse", load("return 0x1p-1") ~= nil and load("return 0x1p-1")() == 0.5)

print("")
if fail > 0 then
  -- os.exit is unavailable here by construction, so raise: the standalone
  -- interpreter exits nonzero on an uncaught error, which is what CI needs.
  error(string.format("%d check(s) FAILED -- lua52f does not match Factorio's sandbox", fail), 0)
end
print("all checks passed")
