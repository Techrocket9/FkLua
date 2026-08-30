#!/usr/bin/env python3
"""Round 4's host-side ABI measurements, driven the way agents/benchmarks.md says.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Every leg runs for the same WALL TIME rather than the same iteration count, the
floor is measured twice bracketing everything it qualifies, and the ratios that
carry the conclusions are between legs measured inside one window -- because
both terms dilate together and a ratio survives a busy machine where a
nanosecond figure does not.

THE ORACLE CAVEAT APPLIES TO EVERY NUMBER HERE and is restated beside each
table: bin/lua52f reads a Lua table 4-6x faster than Factorio does, so a
host-side ratio between two forms differing in TABLE work understates the
in-game difference, and one differing in NON-TABLE work overstates it. The legs
here are paired so that the two sides share as much table work as the question
allows.

  ./scratchpad/r4/bench.py 2>&1 | tee scratchpad/r4/RESULTS.txt
"""

import os
import subprocess
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
FKLUA = os.path.join(ROOT, "bin", "fklua")
TMP = os.path.join(ROOT, "testdata", "tmp", "r4")
HARNESS = os.path.join(ROOT, "scratchpad", "r4", "harness.lua")
ABIDIR = os.path.join(ROOT, "runtime", "lua")

# How long a timed leg runs. Same rule, same reason, as
# internal/guest/callcost_test.go's callCostTargetMS.
TARGET_MS = 120.0
PILOT = 3
MAX_REPS = 20_000_000


def q(s):
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


_MEM = None
_HARNESS = None


def script(leg, reps, count, guicount, verify=False):
    global _MEM, _HARNESS
    if _MEM is None:
        with open(os.path.join(TMP, "mem.lua")) as f:
            _MEM = f.read()
        with open(HARNESS) as f:
            _HARNESS = f.read()
    # The chunk is INLINED rather than loaded from a file: Factorio's sandbox
    # has no `dofile` or `loadfile` and bin/lua52f is patched to match, which is
    # the oracle being right rather than a limitation.
    return "\n".join([
        f"ABIDIR = {q(ABIDIR)}",
        f"LEG = {q(leg)}",
        f"REPS = {reps}",
        f"COUNT = {count}",
        f"GUICOUNT = {guicount}",
        f"VERIFY = {'true' if verify else 'false'}",
        "MODTAB = (function(...)",
        _MEM,
        "end)({})",
        _HARNESS,
    ])


def run(src):
    p = subprocess.run([LUA, "-"], input=src, capture_output=True, text=True)
    if p.returncode != 0:
        sys.stderr.write(p.stdout + p.stderr)
        raise SystemExit(f"lua52f failed: {p.returncode}")
    return p.stdout.strip()


def timed(leg, reps, count, guicount):
    src = script(leg, reps, count, guicount)
    best = None
    for _ in range(3):
        t0 = time.perf_counter()
        out = run(src)
        el = time.perf_counter() - t0
        if out != "done":
            raise SystemExit(f"leg {leg} did not complete: {out!r}")
        if best is None or el < best:
            best = el
    return best


def per_iter(leg, count, guicount):
    """ns per iteration of `leg`, sized so the leg runs for TARGET_MS."""
    zero = timed(leg, 0, count, guicount)
    reps = PILOT
    elapsed = timed(leg, reps, count, guicount) - zero
    target = TARGET_MS / 1000.0
    for _ in range(4):
        if elapsed >= target / 2 or reps >= MAX_REPS:
            break
        scale = 20.0
        if elapsed > 0:
            scale = min(scale, target / elapsed)
        nxt = int(reps * scale)
        if nxt <= reps:
            nxt = reps * 2
        reps = min(nxt, MAX_REPS)
        elapsed = timed(leg, reps, count, guicount) - zero
    return elapsed / reps * 1e9


def build():
    os.makedirs(TMP, exist_ok=True)
    wat = os.path.join(TMP, "mem.wat")
    with open(wat, "w") as f:
        f.write("(module (memory 16)\n\t(func (export \"f\") (result i32) (i32.const 0)))\n")
    subprocess.run([FKLUA, "compile", wat, "--opt=2", "-o",
                    os.path.join(TMP, "mem.lua")], check=True,
                   stdout=subprocess.DEVNULL)


def main():
    if not os.path.exists(LUA):
        raise SystemExit("bin/lua52f is missing; run: make lua52f")
    if not os.path.exists(FKLUA):
        raise SystemExit("bin/fklua is missing; run: go build -o bin/fklua ./cmd/fklua")
    build()

    N = int(os.environ.get("N", "200"))
    G = int(os.environ.get("G", "50"))

    print("=== anti-vacuity: every leg really did its work ===")
    print(run(script("floor", 0, N, G, verify=True)))
    print()

    # --- 4a ---------------------------------------------------------------
    a_legs = ["lua_poll", "percall", "percall_str", "dispatch",
              "bulk_full", "bulk_encode_rets", "bulk_batchpcall", "bulk_bare", "bulk_bare_u32",
              "bulk_read_only", "bulk_resolve_only", "bulk_ld32_only",
              "bulk_store_f64", "bulk_store_u32", "bulk_direct_inline",
              "bulk_direct", "bulk_direct_raw"]
    fa = per_iter("floor", N, G)
    a = {leg: per_iter(leg, N, G) for leg in a_legs}
    fb = per_iter("floor", N, G)
    floor = (fa + fb) / 2
    aa = abs(fb - fa) / floor if floor else 0.0

    print(f"=== 4a  BULK ATTRIBUTE READ  (N = {N} entities per iteration) ===")
    print(f"  floor (a plain Lua call)   {floor:9.1f} ns   "
          f"(A/A {fa:.0f} and {fb:.0f} ns, spread {aa*100:.1f}% -- this run's resolution)")
    base = a["percall"]
    print(f"  {'leg':<22} {'ns/iter':>12} {'ns/element':>12} {'vs percall':>11} {'vs hand Lua':>12}")
    lua = a["lua_poll"] / N
    for leg in a_legs:
        pe = a[leg] / N
        print(f"  {leg:<22} {a[leg]:12.0f} {pe:12.1f} "
              f"{pe/(base/N):10.3f}x {pe/lua:11.1f}x")

    # --- 4b ---------------------------------------------------------------
    b_legs = ["lua_add", "add_percall", "add_batch_dyn", "add_batch_typed",
              "decode_dyn", "decode_typed", "decode_typed_pooled",
              "decode_typed_strings", "decode_typed_table",
              "decode_typed_interned", "decode_typed_mixed"]
    fc = per_iter("floor", N, G)
    b = {leg: per_iter(leg, N, G) for leg in b_legs}
    fd = per_iter("floor", N, G)
    floor2 = (fc + fd) / 2
    aa2 = abs(fd - fc) / floor2 if floor2 else 0.0

    print()
    print(f"=== 4b  BATCHED GUI ADD  (G = {G} elements per iteration) ===")
    print(f"  floor (a plain Lua call)   {floor2:9.1f} ns   "
          f"(A/A {fc:.0f} and {fd:.0f} ns, spread {aa2*100:.1f}%)")
    baseb = b["add_percall"]
    luab = b["lua_add"] / G
    print(f"  {'leg':<22} {'ns/iter':>12} {'ns/element':>12} {'vs percall':>11} {'vs hand Lua':>12}")
    for leg in b_legs:
        pe = b[leg] / G
        print(f"  {leg:<22} {b[leg]:12.0f} {pe:12.1f} "
              f"{pe/(baseb/G):10.3f}x {pe/luab:11.1f}x")

    print()
    print(f"NOISE: the two A/A spreads are {aa*100:.1f}% and {aa2*100:.1f}%. "
          "A ratio inside them is not a measurement.")

    # --- the amortization sweep ------------------------------------------
    #
    # A bulk crossing has a FIXED cost -- the dispatch a per-call form pays per
    # element -- so the whole claim is that it amortizes. A design that asserted
    # it without measuring it would be asserting the thing most likely to be
    # false at the sizes an author actually uses.
    if os.environ.get("SWEEP", "1") == "1":
        print()
        print("=== 4a  AMORTIZATION: does one crossing pay for itself at small N? ===")
        print(f"  {'N':>6} {'percall ns/el':>15} {'bulk_full ns/el':>17} "
              f"{'bulk_inline ns/el':>19} {'best vs percall':>17}")
        for n in (1, 2, 4, 8, 16, 64, 256, 1024):
            pc = per_iter("percall", n, G) / n
            bf = per_iter("bulk_full", n, G) / n
            bi = per_iter("bulk_direct_inline", n, G) / n
            print(f"  {n:6d} {pc:15.1f} {bf:17.1f} {bi:19.1f} {bi/pc:16.3f}x")


if __name__ == "__main__":
    main()
