#!/usr/bin/env python3
"""Interleaved A/B for the write-barrier candidates, with an A/A floor.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Why not `scripts/bench-guests.sh`: that reports best-of-5 per cell, which is the
right instrument for "what does a mod author get" and the wrong one for "is this
1% real". The differences here are expected to be at or below the noise floor,
so the protocol is the one agents/benchmarks.md's numbers were taken with --
paired samples, interleaved so a machine that gets busy halfway through spoils
both arms equally, ratio of medians, bootstrap CI, and an A/A cell (the same
variant against itself, under two different file names) that says what the
machine's own floor is. A ratio inside the A/A interval is NOT a measurement.

Usage:
    python3 scratchpad/gc/bench.py --guest go [--reps 2] [--samples 12]
    python3 scratchpad/gc/bench.py --opt-kernels
"""

import argparse
import json
import os
import random
import statistics
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import variants  # noqa: E402

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
TMP = os.path.join(ROOT, "testdata", "tmp", "gc")

# (kernel, setup export, setup arg, kernel args) -- identical to
# scripts/bench-guests.sh so a number here is comparable to the published table.
KERNELS = [
    ("pure_sum", "pure_setup", 65536, "40"),
    ("pure_prng", None, 0, "2000000"),
    ("pure_dot", "dot_setup", 32768, "40"),
    ("real_entities", "ents_setup", 20000, "40"),
    ("real_grid", "grid_setup", 200, "200, 20"),
    ("real_names", None, 0, "200000"),
]


# The churn guest calls fk_log, so it needs the import table the bench guests
# do not. Supplying a no-op keeps the measurement about allocation rather than
# about a host call.
IMPORTS = "local imports = { env = { fk_log = function(p, n) end } }\n"


def script(chunk_path, kernel, setup, sn, args, reps):
    head = (
        IMPORTS
        + "local M = (function(...)\n"
        + open(chunk_path).read()
        + "\nend)(imports)\n"
        "if M.exports['_initialize'] then M.exports['_initialize']() end\n"
        "local K = setmetatable({}, {__index=function(_,k) return M.exports[k] end})\n"
    )
    body = f"K['{setup}']({sn})\n" if setup else ""
    body += f"local r for _ = 1, {reps} do r = K['{kernel}']({args}) end\n"
    body += "print(string.format('%.10g', r))\n" if reps else "print('setup-only')\n"
    return head + body


def run_once(path):
    t = time.perf_counter()
    p = subprocess.run([LUA, path], capture_output=True, text=True)
    dt = time.perf_counter() - t
    if p.returncode != 0:
        sys.exit(f"{path} failed:\n{p.stdout}{p.stderr}")
    return dt, p.stdout.strip()


def bootstrap_ratio(a, b, n=4000):
    """CI on median(b)/median(a) by resampling the paired samples."""
    rng = random.Random(12345)
    k = len(a)
    rs = []
    for _ in range(n):
        idx = [rng.randrange(k) for _ in range(k)]
        ma = statistics.median([a[i] for i in idx])
        mb = statistics.median([b[i] for i in idx])
        rs.append(mb / ma)
    rs.sort()
    return rs[int(0.025 * n)], rs[int(0.975 * n)]


def measure(cells, kernel, setup, sn, args, reps, samples):
    """cells: {name: chunk_path}. Returns {name: [per-rep seconds]}."""
    paths, zeros = {}, {}
    for name, chunk in cells.items():
        p = os.path.join(TMP, f"run-{name}-{kernel}.lua")
        open(p, "w").write(script(chunk, kernel, setup, sn, args, reps))
        paths[name] = p
        z = os.path.join(TMP, f"run0-{name}-{kernel}.lua")
        open(z, "w").write(script(chunk, kernel, setup, sn, args, 0))
        zeros[name] = z

    # Fixed overhead (process start, chunk parse, _initialize, setup) is
    # measured per cell, because a variant with more source parses more source.
    zero = {}
    for name in cells:
        zero[name] = min(run_once(zeros[name])[0] for _ in range(5))

    out = {name: [] for name in cells}
    sums = {}
    order = list(cells)
    for i in range(samples):
        for name in (order if i % 2 == 0 else order[::-1]):
            dt, s = run_once(paths[name])
            out[name].append(max(dt - zero[name], 1e-9) / reps)
            sums.setdefault(name, set()).add(s)
    allsums = set()
    for s in sums.values():
        allsums |= s
    if len(allsums) != 1:
        sys.exit(f"{kernel}: checksums differ across variants/runs: {sums}")
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--guest", default="go")
    ap.add_argument("--reps", type=int, default=2)
    ap.add_argument("--samples", type=int, default=11)
    ap.add_argument("--cells",
                    default="base,pageset,card64k,pageset2,flagstore,flagunguard,callstore")
    ap.add_argument("--json", default="")
    ap.add_argument("--churn", action="store_true",
                    help="measure the allocation-churn guest instead of the bench kernels")
    a = ap.parse_args()

    global KERNELS
    if a.churn:
        a.guest = "churn"
        KERNELS = [("churn_events", None, 0, "5000")]

    os.makedirs(TMP, exist_ok=True)
    src = open(os.path.join(TMP, f"k-{a.guest}-t3.lua")).read()
    print("# transform receipt:", variants.counts(src))

    cells = {}
    for name in a.cells.split(","):
        p = os.path.join(TMP, f"v-{a.guest}-{name}.lua")
        open(p, "w").write(variants.VARIANTS[name](src))
        cells[name] = p
    # The A/A cell: base again, under a different name and a different file, so
    # it goes through every step the real cells do.
    aa = os.path.join(TMP, f"v-{a.guest}-aa.lua")
    open(aa, "w").write(src)
    cells["aa"] = aa
    # The wide-store gate (b3) is not a rewrite -- it is what the compiler
    # already emits under --persist=packed, so it is a cell when it exists.
    packed = os.path.join(TMP, f"k-{a.guest}-p3.lua")
    if os.path.exists(packed):
        cells["widegate"] = packed

    names = [n for n in cells if n != "base"]
    print(f"\n  reps={a.reps} samples={a.samples} guest={a.guest} -opt=3, table mode")
    hdr = f"  {'kernel':<15}{'base ms':>10}" + "".join(f"{n:>22}" for n in names)
    print(hdr)
    print("  " + "-" * (len(hdr) - 2))
    results = {}
    for kernel, setup, sn, args in KERNELS:
        r = measure(cells, kernel, setup, sn, args, a.reps, a.samples)
        med = {n: statistics.median(v) for n, v in r.items()}
        row = f"  {kernel:<15}{med['base']*1000:>10.2f}"
        results[kernel] = {"ms": {n: med[n] * 1000 for n in med}, "ci": {}}
        for n in names:
            lo, hi = bootstrap_ratio(r["base"], r[n])
            results[kernel]["ci"][n] = [med[n] / med["base"], lo, hi]
            row += f"{med[n]/med['base']:>10.3f}x[{lo:.3f},{hi:.3f}]".rjust(22)
        print(row)
        sys.stdout.flush()
    if a.json:
        json.dump(results, open(a.json, "w"), indent=1)


if __name__ == "__main__":
    main()
