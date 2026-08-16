#!/usr/bin/env python3
"""Shared machinery for the stage-B collector measurements.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Two things live here that stage A's harness did not need.

The first is BUILDING THE ARMS. Stage A produced its variants by rewriting one
emitted chunk, because it was pricing a change to the emitter. Stage B is
pricing a change to the GUEST -- a different allocator, linked in through
-gc=custom -- so every arm is a separate tinygo build and a separate fklua
compile, and the thing that makes the comparison honest is that all of them
come from the SAME Go source with one flag moved.

The second is that bin/lua52f has no `io` and no `os`. It is patched to
Factorio's sandbox on purpose (agents/sandbox.md), so a driver cannot read a
file or a clock: the chunk is embedded into a generated script and the timing
is done by the harness around the process, exactly as scratchpad/gc/bench.py
does. That is also why a COLLECTION PAUSE here is derived from a paired run
against a no-collection control rather than read off a clock inside the guest --
the same derivation agents/gc.md used to get ~10 ms out of the -gc=conservative
reference.
"""

import os
import random
import statistics
import subprocess
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
FKLUA = os.path.join(ROOT, "bin", "fklua")
TMP = os.path.join(ROOT, "testdata", "tmp", "gcb")

# The four arms, and what each one is FOR.
#
#   leaking      the shipping default, and the baseline every ratio is against
#   bump         -gc=custom plumbing with a bump allocator that never frees.
#                agents/gc.md risk 1 says to measure this BEFORE writing a
#                collector, because it isolates the plumbing from the policy.
#   fkgc         the real allocator: size-class free lists, no collection run
#   conservative TinyGo's own collector, which exists today and costs one build
ARMS = {
    "leaking": dict(gc="leaking", tags=""),
    "bump": dict(gc="custom", tags="fkgcbump"),
    "fkgc": dict(gc="custom", tags=""),
    "conservative": dict(gc="conservative", tags=""),
}

IMPORTS = "local imports = { env = { fk_log = function(p, n) end } }\n"


def sh(cmd, cwd=None):
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if p.returncode != 0:
        sys.exit(f"failed: {' '.join(cmd)}\n{p.stdout}{p.stderr}")
    return p.stdout


def build(guest_dir, pkg, name, arm, opt="3", persist="table"):
    """tinygo build + fklua compile one arm. Returns the chunk path."""
    os.makedirs(TMP, exist_ok=True)
    a = ARMS[arm]
    wasm = os.path.join(TMP, f"{name}-{arm}.wasm")
    cmd = ["tinygo", "build", "-target=wasm-unknown", "-scheduler=none",
           f"-gc={a['gc']}", "-opt=2"]
    if a["tags"]:
        cmd += ["-tags", a["tags"]]
    cmd += ["-o", wasm, pkg]
    sh(cmd, cwd=guest_dir)
    chunk = os.path.join(TMP, f"{name}-{arm}-o{opt}-{persist}.lua")
    sh([FKLUA, "compile", wasm, f"--opt={opt}", f"--persist={persist}", "-o", chunk])
    return chunk


def head(chunk_path):
    return (IMPORTS
            + "local M = (function(...)\n" + open(chunk_path).read() + "\nend)(imports)\n"
            "if M.exports['_initialize'] then M.exports['_initialize']() end\n"
            "local K = setmetatable({}, {__index=function(_,k) return M.exports[k] end})\n"
            "local WORDS = function() return M.memio.size() / 4 end\n")


def write(path, body):
    open(path, "w").write(body)
    return path


def run_once(path):
    t = time.perf_counter()
    p = subprocess.run([LUA, path], capture_output=True, text=True)
    dt = time.perf_counter() - t
    if p.returncode != 0:
        sys.exit(f"{path} failed:\n{p.stdout}{p.stderr}")
    return dt, p.stdout.strip()


def bootstrap_ratio(a, b, n=4000):
    rng = random.Random(12345)
    k = len(a)
    rs = []
    for _ in range(n):
        idx = [rng.randrange(k) for _ in range(k)]
        rs.append(statistics.median([b[i] for i in idx]) /
                  statistics.median([a[i] for i in idx]))
    rs.sort()
    return rs[int(0.025 * n)], rs[int(0.975 * n)]


def paired(cells, samples, checksum_gate=True):
    """cells: {name: script_path}. Interleaved, paired, checksum-gated.

    Returns ({name: [seconds]}, {name: stdout}). Interleaving matters: a machine
    that gets busy halfway through spoils both arms equally, which is the only
    reason a ratio of medians is trustworthy at this resolution.
    """
    out = {n: [] for n in cells}
    seen = {}
    order = list(cells)
    for i in range(samples):
        for name in (order if i % 2 == 0 else order[::-1]):
            dt, s = run_once(cells[name])
            out[name].append(dt)
            seen.setdefault(name, set()).add(s)
    if checksum_gate:
        allsums = set()
        for s in seen.values():
            allsums |= s
        if len(allsums) != 1:
            sys.exit("outputs differ across arms/runs -- a variant that computes "
                     f"a different answer is not a faster variant:\n{seen}")
    return out, {n: sorted(v)[0] for n, v in seen.items()}
