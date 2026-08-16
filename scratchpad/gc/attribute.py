#!/usr/bin/env python3
"""Where the armed page set's cost goes: stores, or page CHANGES?

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

The two-compare fast path is cheap and MEMPACK.mark is not, so a barrier built
on the page set is only as expensive as its page-change RATE. This counts both,
per kernel, by wrapping MEMPACK.mark and the store leaves in the emitted chunk.
It also reports how many distinct pages a kernel touches, which is what a
collection step would have to re-scan.
"""

import os
import subprocess
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
TMP = os.path.join(ROOT, "testdata", "tmp", "gc")

KERNELS = [
    ("pure_sum", "pure_setup", 65536, "40"),
    ("pure_prng", None, 0, "2000000"),
    ("pure_dot", "dot_setup", 32768, "40"),
    ("real_entities", "ents_setup", 20000, "40"),
    ("real_grid", "grid_setup", 200, "200, 20"),
    ("real_names", None, 0, "200000"),
]

COUNTERS = """
local MARKS, STORES = 0, 0
"""

# Counting wrappers, spliced in after the MEMPACK block closes.
WRAP = """
do
  local real = MEMPACK.mark
  MEMPACK.mark = function(a, b) MARKS = MARKS + 1 return real(a, b) end
end
"""


def instrument(src):
    src = src.replace("local MEMDIRTY = false", COUNTERS + "local MEMDIRTY = true", 1)
    # After the `do ... end` block that fills MEMPACK in, which ends with the
    # restore function; the marker is the next top-level comment.
    anchor = "-- Guest memory into a Lua string."
    assert anchor in src
    src = src.replace(anchor, WRAP + anchor, 1)
    # A store counter on each of the three leaves plus the inlined form.
    src = src.replace(
        "local function st8b(mem, size, a, v)\n",
        "local function st8b(mem, size, a, v)\n  STORES = STORES + 1\n", 1)
    src = src.replace(
        "local function st16(mem, size, a, v)\n",
        "local function st16(mem, size, a, v)\n  STORES = STORES + 1\n", 1)
    src = src.replace(
        "local function st32(mem, size, a, v)\n",
        "local function st32(mem, size, a, v)\n  STORES = STORES + 1\n", 1)
    # Export the counters through the persist surface, which every mode has.
    src = src.replace(
        "    globals = function(t)",
        "    gcstats = function() return MARKS, STORES, DPN_peek() end,\n    globals = function(t)", 1)
    src = src.replace(WRAP, WRAP + "\nfunction DPN_peek() return 0 end\n", 1)
    return src


def main():
    src = open(os.path.join(TMP, "k-go-t3.lua")).read()
    path = os.path.join(TMP, "v-go-count.lua")
    open(path, "w").write(instrument(src))
    print(f"  {'kernel':<15}{'mark() calls':>14}{'leaf stores':>13}{'marks/ms':>11}")
    print("  " + "-" * 53)
    for kernel, setup, sn, args in KERNELS:
        run = os.path.join(TMP, f"count-{kernel}.lua")
        body = "local M = (function(...)\n" + open(path).read() + "\nend)({})\n"
        body += "M.exports['_initialize']()\n"
        if setup:
            body += f"M.exports['{setup}']({sn})\n"
        body += "local m0 = M.persist.gcstats()\n"
        body += f"M.exports['{kernel}']({args})\n"
        body += "local m1, s1 = M.persist.gcstats()\n"
        body += "print(m1 - m0, s1)\n"
        open(run, "w").write(body)
        p = subprocess.run([LUA, run], capture_output=True, text=True)
        if p.returncode != 0:
            sys.exit(p.stdout + p.stderr)
        marks, stores = p.stdout.split()
        print(f"  {kernel:<15}{int(marks):>14,}{int(stores):>13,}")


if __name__ == "__main__":
    main()
