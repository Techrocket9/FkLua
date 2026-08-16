#!/usr/bin/env python3
"""Count calls to every guest function in an emitted chunk.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

bin/lua52f is Factorio's sandbox, so there is no debug.sethook and no sampling
profiler to reach for. What there is instead is the instrument this project
already uses for questions like this one: rewrite the emitted chunk. Each
`F[n] = function(` gets a counter bump, the `-- name (sig)` comment above it
supplies the name, and the totals come out at the end.

Call counts are not times. They are enough to answer "is this function being
called far more often than the design says it should be", which is the question
an allocator that is 1.7x too slow poses first.

    python3 scratchpad/gc/callcount.py <chunk.lua> "<lua body>" [--top 25]
"""

import argparse
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gclib  # noqa: E402

DEF = re.compile(r"^F\[(\d+)\] = function\(", re.M)
NAME = re.compile(r"^-- (\S+) \(", re.M)


def instrument(src):
    names = {}
    lines = src.split("\n")
    out = []
    last_comment = None
    for ln in lines:
        m = NAME.match(ln)
        if m:
            last_comment = m.group(1)
        d = DEF.match(ln)
        if d:
            idx = d.group(1)
            names[idx] = last_comment or f"F[{idx}]"
            ln = ln + f" CNT[{idx}] = CNT[{idx}] + 1"
        out.append(ln)
    # A GLOBAL, because the chunk ends in its own `return {...}` and anything
    # appended after that is unreachable.
    head = "CNT = setmetatable({}, {__index = function() return 0 end})\n"
    return head + "\n".join(out), names


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("chunk")
    ap.add_argument("body")
    ap.add_argument("--top", type=int, default=25)
    a = ap.parse_args()

    src, names = instrument(open(a.chunk).read())
    script = (gclib.IMPORTS + "local M = (function(...)\n" + src + "\nend)(imports)\n"
              "if M.exports['_initialize'] then M.exports['_initialize']() end\n"
              "local K = setmetatable({}, {__index=function(_,k) return M.exports[k] end})\n"
              + a.body +
              "\nlocal t = {}\nfor k, v in pairs(CNT) do t[#t+1] = {k, v} end\n"
              "table.sort(t, function(x, y) return x[2] > y[2] end)\n"
              "for i = 1, #t do print(t[i][1] .. ' ' .. t[i][2]) end\n")
    p = os.path.join(gclib.TMP, "callcount.lua")
    open(p, "w").write(script)
    r = subprocess.run([gclib.LUA, p], capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(r.stdout + r.stderr)
    rows = []
    for line in r.stdout.strip().split("\n"):
        parts = line.split()
        if len(parts) == 2 and parts[1].isdigit():
            rows.append((names.get(parts[0], parts[0]), int(parts[1])))
        else:
            print(line)
    for name, n in rows[:a.top]:
        print(f"{n:>12,}  {name}")


if __name__ == "__main__":
    main()
