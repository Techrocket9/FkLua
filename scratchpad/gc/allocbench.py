#!/usr/bin/env python3
"""The stage-B go/no-go: what the ALLOCATION PATH costs, before any collector runs.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

agents/gc.md's risk 1 and stage B's second kill criterion:

    -gc=leaking's alloc is a bump pointer; a free-list allocator is a walk, and
    that cost is paid by every allocation whether or not a collection is
    running -- by a guest that opted in, at idle, forever. [...] Threshold:
    >10% on real_entities and churn against the same guest built -gc=leaking.
    Measure it against -gc=conservative too.

No collection runs in ANY cell here. That is the whole point: this is the cost a
guest pays for having opted in, on a tick where nothing is collected, which is
almost every tick.

The arms decompose the cost rather than reporting one number for it:

    leaking       the baseline, and the shipping default
    bump          -gc=custom's plumbing with a bump allocator that never frees.
                  The gap between this and leaking is the SEAM; the gap between
                  this and fkgc is the POLICY.
    fkgc          the real size-class allocator
    conservative  TinyGo's own collector, which does collect and is here as the
                  upper bound that exists today

Protocol as agents/benchmarks.md requires and stage A's bench.py established:
paired, interleaved, ratio of medians, bootstrap CI, and an A/A cell -- the
baseline arm against itself under a second file name -- because a ratio inside
the A/A interval is not a measurement.

    python3 scratchpad/gc/allocbench.py [--samples 11] [--reps 2]
"""

import argparse
import os
import statistics
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gclib  # noqa: E402

# Identical to scripts/bench-guests.sh and to stage A's bench.py, so a number
# here is comparable to the published table.
KERNELS = [
    ("pure_sum", "pure_setup", 65536, "40"),
    ("real_entities", "ents_setup", 20000, "40"),
    ("real_grid", "grid_setup", 200, "200, 20"),
    ("real_names", None, 0, "200000"),
]


def body(kernel, setup, sn, args, reps):
    b = f"K['{setup}']({sn})\n" if setup else ""
    if reps:
        b += f"local r for _ = 1, {reps} do r = K['{kernel}']({args}) end\n"
        b += "print(string.format('%.10g', r))\n"
    else:
        b += "print('setup-only')\n"
    return b


def cell(chunks, name, kernel, setup, sn, args, reps, tag=""):
    p = os.path.join(gclib.TMP, f"ab-{name}{tag}-{kernel}.lua")
    return gclib.write(p, gclib.head(chunks[name]) + body(kernel, setup, sn, args, reps))


def run(chunks, arms, kernels, reps, samples, label):
    print(f"\n  {label} -- reps={reps} samples={samples}, -opt=3 --persist=table")
    names = [a for a in arms if a != "leaking"] + ["aa"]
    hdr = f"  {'kernel':<15}{'leaking ms':>12}" + "".join(f"{n:>22}" for n in names)
    print(hdr)
    print("  " + "-" * (len(hdr) - 2))
    for kernel, setup, sn, args in kernels:
        cells = {a: cell(chunks, a, kernel, setup, sn, args, reps) for a in arms}
        # The A/A cell: the baseline again, under a different name and a
        # different file, so it goes through every step the real cells do.
        cells["aa"] = cell(chunks, "leaking", kernel, setup, sn, args, reps, tag="-aa")
        zeros = {a: cell(chunks, "leaking" if a == "aa" else a, kernel, setup, sn, args, 0,
                         tag="-z" + a) for a in cells}
        zero = {a: min(gclib.run_once(zeros[a])[0] for _ in range(5)) for a in cells}

        raw, _ = gclib.paired(cells, samples)
        per = {a: [max(t - zero[a], 1e-9) / reps for t in v] for a, v in raw.items()}
        med = {a: statistics.median(v) for a, v in per.items()}
        row = f"  {kernel:<15}{med['leaking']*1000:>12.2f}"
        for n in names:
            lo, hi = gclib.bootstrap_ratio(per["leaking"], per[n])
            row += f"{med[n]/med['leaking']:>10.3f}x[{lo:.3f},{hi:.3f}]".rjust(22)
        print(row)
        sys.stdout.flush()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--samples", type=int, default=11)
    ap.add_argument("--reps", type=int, default=2)
    ap.add_argument("--arms", default="leaking,bump,fkgc,conservative")
    a = ap.parse_args()
    arms = a.arms.split(",")

    bench = os.path.join(gclib.ROOT, "bench", "guests", "go")
    guest = os.path.join(gclib.ROOT, "guest", "go")

    print("=== building the arms ===")
    kchunks = {arm: gclib.build(bench, ".", "k-go", arm) for arm in arms}
    cchunks = {arm: gclib.build(guest, "./examples/churn", "churn", arm) for arm in arms}

    run(kchunks, arms, KERNELS, a.reps, a.samples, "bench guest (Go)")
    run(cchunks, arms, [("churn_events", None, 0, "5000")], 1, max(9, a.samples // 2),
        "allocation-churn guest")


if __name__ == "__main__":
    main()
