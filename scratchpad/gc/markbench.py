#!/usr/bin/env python3
"""Times markloop.lua and turns it into pages/ms and ticks-per-16-MiB-heap.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Same protocol as bench.py: the per-rep cost is (time at REPS - time at 0)/REPS,
best of RUNS, so process startup, the chunk parse and the MEM fill -- which is
O(WORDS) and would otherwise swamp a small span -- are subtracted exactly.
`clear` is subtracted from `scan`/`mark`/`markdrain` on top of that, because
those variants reset the bitmap once per rep and the reset is not marking.
"""

import os
import subprocess
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
SRC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "markloop.lua")
RUNS = int(os.environ.get("RUNS", "5"))


def once(variant, words, reps, dens):
    best, out = 1e9, None
    for _ in range(RUNS):
        t = time.perf_counter()
        p = subprocess.run([LUA, SRC, variant, str(words), str(reps), str(dens)],
                           capture_output=True, text=True)
        dt = time.perf_counter() - t
        if p.returncode != 0:
            sys.exit(f"{variant}: {p.stdout}{p.stderr}")
        best, out = min(best, dt), p.stdout.strip()
    return best, out


def per_rep(variant, words, reps, dens):
    hi, sums = once(variant, words, reps, dens)
    lo, _ = once(variant, words, 0, dens)
    return (hi - lo) / reps, sums


def main():
    words = int(os.environ.get("WORDS", 262144))     # 1 MiB
    reps = int(os.environ.get("REPS", 8))
    mib = words * 4 / 1048576
    print(f"  span = {words:,} words ({mib:.0f} MiB of linear memory), "
          f"granule 16 B, bitmap 32 bits/slot")
    print(f"  {'density':>8}{'variant':>12}{'ms/pass':>10}{'ns/word':>10}"
          f"{'4KiB pages/ms':>15}{'MiB/ms':>9}")
    print("  " + "-" * 64)
    base = {}
    for dens in (5, 20, 40):
        clear_t, _ = per_rep("clear", words, reps, dens)
        for v in ("scan", "mark", "markdrain", "sweep"):
            t, sums = per_rep(v, words, reps, dens)
            if v != "sweep":
                t = max(t - clear_t, 1e-9)
            nsw = t * 1e9 / words
            pages = (words * 4 / 4096) / (t * 1000)
            print(f"  {dens:>7}%{v:>12}{t*1000:>10.2f}{nsw:>10.1f}{pages:>15.0f}"
                  f"{mib/(t*1000):>9.3f}")
            base[(dens, v)] = t
        print()
    d = base[(20, "markdrain")]
    print(f"  at 20% pointer density, markdrain: a 16 MiB heap is "
          f"{16/ (words*4/1048576) * d * 1000:.0f} ms of marking")
    s = base[(20, "sweep")]
    print(f"  at 20% pointer density, sweep:     a 16 MiB heap is "
          f"{16/ (words*4/1048576) * s * 1000:.0f} ms of sweeping")


if __name__ == "__main__":
    main()
