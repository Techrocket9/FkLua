#!/usr/bin/env python3
"""The same barrier candidates against `bench --opt`'s .wat kernels.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

bench/wasm is the only thing in the repo that measures a PASS, so it is where a
barrier's cost per store shows up undiluted by a toolchain's runtime. It is also
where the numbers CLAUDE.md quotes for the inlined store came from (`chase`
0.988x, `sum` no detected change), which is what makes it the right place to ask
what putting a second test back would cost.

Protocol is bench.py's: paired, interleaved, ratio of medians, bootstrap CI, and
an A/A cell.
"""

import os
import statistics
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import variants  # noqa: E402
from bench import bootstrap_ratio, run_once  # noqa: E402

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
FKLUA = os.path.join(ROOT, "bin", "fklua")
WASM = os.path.join(ROOT, "bench", "wasm")
TMP = os.path.join(ROOT, "testdata", "tmp", "gc")

KERNELS = ["sum", "chase", "prng", "dot", "fib", "frame", "count", "constdiv"]
CELLS = ["base", "pageset", "card64k", "pageset2", "flagstore", "flagunguard", "callstore", "aa"]


def compile_kernel(name, persist="table"):
    out = os.path.join(TMP, f"ok-{name}-{persist}.lua")
    subprocess.run([FKLUA, "compile", os.path.join(WASM, name + ".wat"),
                    "--opt=3", f"--persist={persist}", "-o", out],
                   capture_output=True, check=True)
    return open(out).read()


def main():
    samples = int(os.environ.get("SAMPLES", "9"))
    os.makedirs(TMP, exist_ok=True)
    print(f"  bench/wasm kernels, -opt=3, table mode, samples={samples}")
    hdr = f"  {'kernel':<10}{'base ms':>9}" + "".join(f"{c:>10}" for c in CELLS[1:] + ["widegate"])
    print(hdr)
    print("  " + "-" * (len(hdr) - 2))
    for name in KERNELS:
        src = compile_kernel(name)
        driver = open(os.path.join(WASM, name + ".lua")).read()
        cells = {}
        for c in CELLS:
            try:
                s = src if c == "aa" else variants.VARIANTS[c](src)
            except AssertionError:
                # A kernel the transform has nothing to touch -- prng has no
                # memory at all, so there is no inlined store to un-inline.
                # Reported as base rather than skipped, which is what it is.
                s = src
            p = os.path.join(TMP, f"ok-{name}-{c}.lua")
            open(p, "w").write("local M = (function(...)\n" + s + "\nend)()\n" + driver)
            cells[c] = p
        pw = os.path.join(TMP, f"ok-{name}-widegate.lua")
        open(pw, "w").write("local M = (function(...)\n" + compile_kernel(name, "packed")
                            + "\nend)()\n" + driver)
        cells["widegate"] = pw

        # Process start plus the chunk PARSE, per cell -- a variant with more
        # source parses more source, and at these ratios that is not negligible.
        zero = {}
        for c, p in cells.items():
            z = p.replace(".lua", "-zero.lua")
            body = open(p).read().split("\nend)()\n")[0] + "\nend)()\n"
            open(z, "w").write(body)
            zero[c] = min(run_once(z)[0] for _ in range(5))

        times = {c: [] for c in cells}
        sums = set()
        order = list(cells)
        for i in range(samples):
            for c in (order if i % 2 == 0 else order[::-1]):
                dt, out = run_once(cells[c])
                times[c].append(max(dt - zero[c], 1e-9))
                sums.add(out)
        if len(sums) != 1:
            sys.exit(f"{name}: checksums differ: {sums}")
        med = {c: statistics.median(v) for c, v in times.items()}
        row = f"  {name:<10}{med['base']*1000:>9.1f}"
        for c in list(cells)[1:]:
            lo, hi = bootstrap_ratio(times["base"], times[c])
            row += f"{med[c]/med['base']:>10.3f}"
        print(row)
        # The interval matters more than the point estimate at this scale, so it
        # gets its own line rather than being squeezed into the table.
        det = "            "
        for c in list(cells)[1:]:
            lo, hi = bootstrap_ratio(times["base"], times[c])
            det += f"[{lo:.3f},{hi:.3f}] "
        print(det)
        sys.stdout.flush()


if __name__ == "__main__":
    main()
