#!/usr/bin/env python3
"""How many words of a real Go guest heap LOOK like heap pointers.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Conservative marking treats any in-range word as a pointer, and on this target
the range is unusually permissive: the heap starts a few tens of KiB above zero
and can run to tens of MiB, so essentially every medium-sized integer a Go
program holds -- a slice length, a map hash, a loop bound, an entity id -- is
inside it. That is the input to the biggest risk in agents/gc.md, and it can be
measured before any collector exists: run the churn guest, then read its linear
memory straight out of the live word table and count.

Three numbers, because they answer three different questions:

  in-range            what a bare range test would treat as a pointer
  granule-aligned     what a test that also requires GRAN alignment would keep,
                      which is what a base-pointer-only collector accepts
  distinct targets    how many separate granules those words point AT, which
                      bounds how much a single false pointer can retain

The third is the one that matters: false retention is bad in proportion to how
much a false pointer drags in, and that is a property of the object graph rather
than of the density.
"""

import os
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
TMP = os.path.join(ROOT, "testdata", "tmp", "gc")

DRIVER = """
local imports = { env = { fk_log = function(p, n) end } }
local M = (function(...)
%s
end)(imports)
M.exports['_initialize']()
local base = M.exports['churn_heap_top']()
M.exports['churn_events'](%d)
local top = M.exports['churn_heap_top']()
local MEM = M.persist.memory()
local GRAN = 16
local lo, hi = base, top
local total, inrange, aligned = 0, 0, 0
local targets = {}
local ntarget = 0
for i = lo / 4 + 1, hi / 4 do
  local w = MEM[i]
  total = total + 1
  if w ~= nil and w >= lo and w < hi then
    inrange = inrange + 1
    if w %% GRAN == 0 then aligned = aligned + 1 end
    local g = (w - w %% GRAN) / GRAN
    if not targets[g] then targets[g] = true ntarget = ntarget + 1 end
  end
end
print(string.format("%%d %%d %%d %%d %%d %%d", lo, hi, total, inrange, aligned, ntarget))
"""


def main():
    events = int(sys.argv[1]) if len(sys.argv) > 1 else 5000
    src = open(os.path.join(TMP, "k-churn-t3.lua")).read()
    p = os.path.join(TMP, "falseptr.lua")
    open(p, "w").write(DRIVER % (src, events))
    r = subprocess.run([LUA, p], capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(r.stdout + r.stderr)
    lo, hi, total, inrange, aligned, ntarget = (int(x) for x in r.stdout.split())
    heap = hi - lo
    print(f"  churn, {events:,} events")
    print(f"  heap                 {lo:,} .. {hi:,}  ({heap/1048576:.2f} MiB, "
          f"{total:,} words)")
    print(f"  in-range words       {inrange:,}  ({100*inrange/total:.1f}% of the heap)")
    print(f"  ...granule-aligned   {aligned:,}  ({100*aligned/total:.1f}%)")
    print(f"  distinct targets     {ntarget:,}  of {heap//16:,} granules "
          f"({100*ntarget/(heap//16):.1f}%)")


if __name__ == "__main__":
    main()
