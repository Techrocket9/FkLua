-- Do the two spellings of the inlined 4-byte store compile to the SAME
-- bytecode? Dump each with string.dump and compare everything the VM executes
-- -- header, prototype header, code array, constants, nested protos and
-- upvalue descriptors -- stopping before DumpDebug, which is where the source
-- name and the per-instruction LINE NUMBERS live and is the only part two
-- different spellings of one program are expected to differ in. The proto's
-- own linedefined/lastlinedefined pair is skipped for the same reason: the
-- block form's function ends two lines later, which is the whole point.
--
-- Run under bin/lua52f. There is no `io` in the sandbox, so this compares in
-- Lua and prints a verdict rather than writing files out.

local prologue = "local MEM, MEMSIZE, SHBOUND, S1, st32, trap_oob, MEMDIRTY, DPLO, DPHI, MEMPACK\nreturn function(v0, v1)\n  local t0, t1\n  t0 = v0\n"
local epilogue = "end\n"
local cond = "t0 >= 0 and t0 + 4 <= SHBOUND and t0 % 4 == 0"
local st   = "S1[t0 / 4 + 1] = v1 % 4294967296.0"
local mk   = "if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end"
local slow = "    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end\n"
          .. "    if t0 % 4 == 0 then MEM[(t0 - t0 % 2097152) / 2097152 + 1][t0 % 2097152 / 4 + 1] = v1 % 4294967296.0 else st32(MEM, MEMSIZE, t0, v1) end\n"

-- table / none: no barrier at all, since the mark is gated on marksPages().
local block_t  = prologue.."  if "..cond.." then\n    "..st.."\n  else\n"..slow.."  end\n"..epilogue
local opener_t = prologue.."  if "..cond.." then "..st.." else\n"..slow.."  end\n"..epilogue
-- packed / collected: the barrier rides the opener line with the store.
local block_p  = prologue.."  if "..cond.." then\n    "..mk.."\n    "..st.."\n  else\n    "..mk.."\n"..slow.."  end\n"..epilogue
local opener_p = prologue.."  if "..cond.." then "..mk.." "..st.." else\n    "..mk.."\n"..slow.."  end\n"..epilogue

local function u32(s, p)
  local a, b, c, d = s:byte(p, p + 3)
  return a + b * 256 + c * 65536 + d * 16777216, p + 4
end

-- Returns the offset one past the last executable byte, i.e. the start of
-- DumpDebug for a top-level proto.
local function execend(s)
  local p = 19                       -- 12-byte header + 6-byte LUAC_TAIL
  p = p + 8                          -- linedefined, lastlinedefined
  p = p + 3                          -- numparams, is_vararg, maxstacksize
  local n; n, p = u32(s, p); p = p + 4 * n            -- code
  n, p = u32(s, p)                                    -- constants
  for _ = 1, n do
    local t = s:byte(p); p = p + 1
    if t == 1 then p = p + 1
    elseif t == 3 then p = p + 8
    elseif t == 4 then local L; L, p = u32(s, p); p = p + 4 + L end
  end
  n, p = u32(s, p)
  if n ~= 0 then error("nested protos: this comparison assumes none") end
  n, p = u32(s, p); p = p + 2 * n                     -- upvalue descriptors
  return p
end

local function exec(src)
  local d = string.dump(assert(load(src, "=store"))())
  -- 27 is one past linedefined/lastlinedefined; 19..26 are those two ints.
  return d:sub(27, execend(d) - 1), #d
end

local function cmp(name, a, b)
  local ea, na = exec(a)
  local eb, nb = exec(b)
  print(string.format("%-22s executable %d vs %d bytes, %s; whole dump %d vs %d "
    .. "(the difference is lineinfo and the source name)",
    name, #ea, #eb, ea == eb and "IDENTICAL" or "DIFFERENT", na, nb))
  return ea == eb
end

local ok = true
ok = cmp("table / none mode", block_t, opener_t) and ok
ok = cmp("packed / collected", block_p, opener_p) and ok
print(ok and "VERDICT: same bytecode, fewer lines" or "VERDICT: BYTECODE MOVED")

-- THE CONTROL, so that "IDENTICAL" above means something. A comparison that
-- cannot say DIFFERENT is not a comparison: drop the store's `% 2^32` -- one
-- real change to the program, invisible in the line count -- and this must
-- report DIFFERENT.
local dropped = prologue.."  if "..cond.." then S1[t0 / 4 + 1] = v1 else\n"..slow.."  end\n"..epilogue
local discriminates = not cmp("CONTROL: no % 2^32", block_t, dropped)
print(discriminates and "CONTROL OK: the comparison can say DIFFERENT"
  or "CONTROL FAILED: the comparison is vacuous")
