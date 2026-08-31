#!/usr/bin/env python3
"""What a TYPED ARGUMENT BLOCK costs against the tier-2 map, on the shipped path.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Round 4b measured PROTOTYPES of two encodings, because the feature did not
exist; this measures what round 2 shipped -- the real runtime/lua/fk_abi.lua
against the real generated `sig` for LuaGuiElement::add, M.call against
M.call_typed. The method is 4b's: every leg runs for the same WALL TIME rather
than the same iteration count, the floor is measured twice bracketing everything
it qualifies, and the ratios that carry the conclusions are between legs measured
inside one window.

THE ORACLE CAVEAT APPLIES AND POINTS ONE WAY. bin/lua52f reads a Lua table 4-6x
faster than Factorio does and both legs here are table-dominated, so the ratio
between them is the conservative half of the estimate. The absolute nanoseconds
are not a game figure.

  ./scratchpad/r2/bench.py 2>&1 | tee scratchpad/r2/RESULTS.txt
"""

import os
import subprocess
import sys
import time

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
LUA = os.path.join(ROOT, "bin", "lua52f")
FKLUA = os.path.join(ROOT, "bin", "fklua")
TMP = os.path.join(ROOT, "testdata", "tmp", "r2")
HARNESS = os.path.join(ROOT, "scratchpad", "r2", "harness.lua")
DUMP = os.path.join(ROOT, "scratchpad", "r2", "dumpmembers.go")
ABIDIR = os.path.join(ROOT, "runtime", "lua")

# LuaGuiElement::add at the default pin. Dumped rather than written down, so a
# pin move changes the number in one place and the harness reads the real sig.
MID = 1932

TARGET_MS = 120.0
PILOT = 3
MAX_REPS = 20_000_000


def q(s):
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


_MEM = None
_MEMBERS = None
_HARNESS = None


def script(leg, reps, verify=False):
    global _MEM, _MEMBERS, _HARNESS
    if _MEM is None:
        with open(os.path.join(TMP, "mem.lua")) as f:
            _MEM = f.read()
        with open(os.path.join(TMP, "members.lua")) as f:
            _MEMBERS = f.read()
        with open(HARNESS) as f:
            _HARNESS = f.read()
    # Both chunks are INLINED rather than loaded from a file: Factorio's sandbox
    # has no `dofile` or `loadfile` and bin/lua52f is patched to match.
    return "\n".join([
        f"ABIDIR = {q(ABIDIR)}",
        f"LEG = {q(leg)}",
        f"REPS = {reps}",
        f"VERIFY = {'true' if verify else 'false'}",
        "MODTAB = (function(...)",
        _MEM,
        "end)({})",
        "MEMBERS = (function()",
        _MEMBERS,
        "end)()",
        _HARNESS,
    ])


def run(src):
    p = subprocess.run([LUA, "-"], input=src, capture_output=True, text=True)
    if p.returncode != 0:
        sys.stderr.write(p.stdout + p.stderr)
        raise SystemExit(f"lua52f failed: {p.returncode}")
    return p.stdout.strip()


def timed(leg, reps):
    src = script(leg, reps)
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


def per_iter(leg):
    zero = timed(leg, 0)
    reps = PILOT
    elapsed = timed(leg, reps) - zero
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
        elapsed = timed(leg, reps) - zero
    return elapsed / reps * 1e9


def build():
    os.makedirs(TMP, exist_ok=True)
    wat = os.path.join(TMP, "mem.wat")
    with open(wat, "w") as f:
        f.write("(module (memory 16)\n\t(func (export \"f\") (result i32) (i32.const 0)))\n")
    subprocess.run([FKLUA, "compile", wat, "--opt=2", "-o",
                    os.path.join(TMP, "mem.lua")], check=True,
                   stdout=subprocess.DEVNULL)
    with open(os.path.join(TMP, "members.lua"), "w") as f:
        subprocess.run(["go", "run", DUMP, str(MID)], cwd=ROOT, check=True, stdout=f)


def main():
    if not os.path.exists(LUA):
        raise SystemExit("bin/lua52f is missing; run: make lua52f")
    if not os.path.exists(FKLUA):
        raise SystemExit("bin/fklua is missing; run: go build -o bin/fklua ./cmd/fklua")
    build()

    print("=== anti-vacuity: both paths ran, and the two decodes agree ===")
    print(run(script("floor", 0, verify=True)))
    print()

    legs = ["lua_add",
            "call_dyn", "call_typed", "decode_dyn", "decode_typed",
            "call_dyn_flat", "call_typed_flat", "decode_dyn_flat", "decode_typed_flat",
            "decode_dyn_strvals", "decode_dyn_numvals"]
    # Round 4b's re-judgment: a whole WINDOW of 50 elements, per-call against a
    # batch, both over the typed block round 2 shipped. Reported per element, so
    # the column is comparable with everything above it.
    wlegs = ["window_percall_typed", "window_batch_pooled", "window_batch_nopool"]
    fa = per_iter("floor")
    r = {leg: per_iter(leg) for leg in legs}
    fb = per_iter("floor")
    floor = (fa + fb) / 2
    aa = abs(fb - fa) / floor if floor else 0.0

    print("=== ONE GUI ELEMENT SPEC, TWO WIRE FORMS, THROUGH THE SHIPPED PATH ===")
    print(f"  floor (a plain Lua call)   {floor:9.1f} ns   "
          f"(A/A {fa:.0f} and {fb:.0f} ns, spread {aa*100:.1f}% -- this run's resolution)")
    base = r["call_dyn"]
    lua = r["lua_add"]
    print(f"  {'leg':<16} {'ns/element':>12} {'vs call_dyn':>12} {'vs hand Lua':>12}")
    for leg in legs:
        print(f"  {leg:<16} {r[leg]:12.1f} {r[leg]/base:11.3f}x {r[leg]/lua:11.1f}x")
    print()
    print(f"NOISE: the A/A spread is {aa*100:.1f}%. A ratio inside it is not a measurement.")

    # --- round 4b's re-judgment -------------------------------------------
    G = 50
    fc = per_iter("floor")
    w = {leg: per_iter(leg) for leg in wlegs}
    fd = per_iter("floor")
    floor2 = (fc + fd) / 2
    aa2 = abs(fd - fc) / floor2 if floor2 else 0.0
    print()
    print(f"=== A WHOLE WINDOW OF {G} ELEMENTS: PER CALL AGAINST A BATCH ===")
    print(f"  floor (a plain Lua call)   {floor2:9.1f} ns   "
          f"(A/A {fc:.0f} and {fd:.0f} ns, spread {aa2*100:.1f}%)")
    basew = w["window_percall_typed"]
    print(f"  {'leg':<24} {'ns/window':>12} {'ns/element':>12} {'vs per-call typed':>19}")
    for leg in wlegs:
        print(f"  {leg:<24} {w[leg]:12.0f} {w[leg]/G:12.1f} "
              f"{w[leg]/basew:18.3f}x")
    print()
    print("  THE DECISION RULE was: implement fk.batch_add if the batch-and-pool "
          "form is <= 0.6x the per-call TYPED form on this corpus.")
    print(f"  MEASURED: {w['window_batch_pooled']/basew:.3f}x "
          f"(and {w['window_batch_nopool']/basew:.3f}x with the pool ablated, "
          f"so the pool itself is worth "
          f"{w['window_batch_nopool']/w['window_batch_pooled']:.3f}x).")


if __name__ == "__main__":
    main()
