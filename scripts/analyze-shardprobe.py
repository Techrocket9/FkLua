#!/usr/bin/env python3
"""Turn a shardprobe run into the candidate table for agents/sharding.md.

Reads the FKSHARD lines scraped out of factorio-current.log by
scripts/run-shardprobe.sh. Timings arrive as log lines because
helpers.create_profiler() is the only clock in Factorio's sandbox and it
refuses to hand Lua a raw number.

Ratios are always printed against the flat arm measured AT THE SAME MEMORY
SIZE IN THE SAME RUN, and the A/A spread of that arm is printed beside them --
agents/benchmarks.md's rule that a wall-clock number needs a floor measured in
the same run.
"""
import re
import sys

TIME_RE = re.compile(
    r"FKSHARD\t(?P<label>[^\t]+)\t(?P<reps>\d+)\t.*?Duration:\s*(?P<ms>[\d.]+)\s*ms"
)

SIZES = ["2MiB", "3.5MiB", "5MiB", "8MiB"]

CURVE_SIZES = ["1.0MiB", "2.0MiB", "3.0MiB", "3.9MiB",
               "4.000MiB", "4.016MiB", "5.0MiB"]

CURVE = [
    ("c_ctl", "the same loop with the TABLE READ REMOVED (floor)"),
    ("c_ld_flat", "load, flat, shards NOT yet allocated"),
    ("c_ld_flat2", "load, flat, shards also resident (confound check)"),
    ("c_ld_divmod", "load, sharded"),
    ("c_ld_guard_shard", "load, sharded, guard-hoisted"),
    ("c_st_flat", "store, flat"),
    ("c_st_divmod", "store, sharded"),
]

ACCESS = [
    ("ld_ctl", "the same loop with the table read REMOVED (floor)"),
    ("ld_flat", "load, flat MEM[t0/4+1]  (today)"),
    ("ld_divmod", "load, shard select by arithmetic"),
    ("ld_bit32", "load, shard select by bit32"),
    ("ld_basekey", "load, shard by base-address hash"),
    ("ld_guard_upval", "load, guarded, MEM upvalue (today)"),
    ("ld_guard_flat", "load, guarded, flat table in a local"),
    ("ld_guard_shard", "load, guarded, SHARD in a local"),
    ("ld_dispatch_branch_flat", "load, mode branch, flat arm"),
    ("ld_dispatch_branch_shard", "load, mode branch, shard arm"),
    ("ld_dispatch_fn_flat", "load, upvalue fn swap, flat"),
    ("ld_dispatch_fn_shard", "load, upvalue fn swap, shard"),
    ("st_flat", "store, flat  (today)"),
    ("st_divmod", "store, shard select by arithmetic"),
    ("st_guard_flat", "store, guarded, flat in a local"),
    ("st_guard_shard", "store, guarded, SHARD in a local"),
    ("ld_flat_spread", "load, flat, 8 KiB stride over all memory"),
    ("ld_divmod_spread", "load, sharded, 8 KiB stride over all memory"),
]

BULK = [
    ("build_flat", "build from empty, one flat table  (= per-LOAD cost)"),
    ("build_shard", "build from empty, 2 MiB shards"),
    ("fill_flat", "mem_fill 1 MiB, flat"),
    ("fill_shard_split", "mem_fill 1 MiB, split at shard boundaries"),
]


def main():
    path = sys.argv[1]
    t = {}
    for line in open(path, encoding="utf-8", errors="replace"):
        m = TIME_RE.search(line)
        if m:
            reps, ms = int(m.group("reps")), float(m.group("ms"))
            # A repeated run of the same label keeps the FASTEST, which is the
            # quiet slice; --benchmark-runs > 1 replays the whole probe.
            lab = m.group("label")
            rec = {"reps": reps, "ms": ms, "ns": ms * 1e6 / reps}
            if lab not in t or rec["ms"] < t[lab]["ms"]:
                t[lab] = rec
    if not t:
        sys.exit("no FKSHARD timing lines found in " + path)

    sizes = [s for s in SIZES if ("ld_flat/" + s) in t]

    print()
    print("A/A floor (the flat load measured first and last at each size)")
    print("  size      head ns   tail ns   spread")
    for s in sizes:
        h, l = t.get("aa_head/" + s), t.get("aa_tail/" + s)
        if not (h and l):
            continue
        spread = abs(l["ns"] - h["ns"]) / h["ns"] * 100
        print("  %-8s %8.1f  %8.1f   %5.2f%%" % (s, h["ns"], l["ns"], spread))

    csizes = [s for s in CURVE_SIZES if ("c_ld_flat/" + s) in t]
    if csizes:
        print()
        print("THE CURVE, ns per access  (flat arms measured BEFORE the shards exist)")
        hdr = "  %-48s" % "shape"
        for s in csizes:
            hdr += "  %9s" % s
        print(hdr)
        for key, desc in CURVE:
            row = "  %-48s" % desc
            for s in csizes:
                r = t.get(key + "/" + s)
                row += "  %9s" % ("-" if not r else "%.1f" % r["ns"])
            print(row)
        row = "  %-48s" % "-> sharded / flat"
        for s in csizes:
            f, d = t.get("c_ld_flat/" + s), t.get("c_ld_divmod/" + s)
            row += "  %9s" % ("-" if not (f and d) else "%.2fx" % (d["ns"] / f["ns"]))
        print(row)
        row = "  %-48s" % "-> rebuild, sharded, ms"
        for s in csizes:
            b = t.get("c_build_shard/" + s)
            row += "  %9s" % ("-" if not b else "%.0f" % b["ms"])
        print(row)

    print()
    print("PER-ACCESS COST, ns  (ratio against ld_flat at the same size)")
    hdr = "  %-44s" % "shape"
    for s in sizes:
        hdr += "  %18s" % s
    print(hdr)
    for key, desc in ACCESS:
        row = "  %-44s" % desc
        for s in sizes:
            r = t.get(key + "/" + s)
            base = t.get(("st_flat/" if key.startswith("st_") else "ld_flat/") + s)
            if not r:
                row += "  %18s" % "-"
            else:
                ratio = r["ns"] / base["ns"] if base else float("nan")
                row += "  %10.1f %6.2fx" % (r["ns"], ratio)
        print(row)

    print()
    print("BULK, total ms")
    hdr = "  %-52s" % "shape"
    for s in sizes:
        hdr += "  %11s" % s
    print(hdr)
    for key, desc in BULK:
        row = "  %-52s" % desc
        for s in sizes:
            r = t.get(key + "/" + s)
            row += "  %11s" % ("-" if not r else "%.1f" % r["ms"])
        print(row)

    # PAIRED. Each repetition contributes one ratio; what is reported is the
    # median ratio and the min/max across repetitions, because the effect here
    # is the same order as the run-to-run spread.
    psizes = []
    for s in SIZES + CURVE_SIZES + ["8.0MiB"]:
        if (("p_ld_flat.1/" + s) in t or ("a_ld_flat.1/" + s) in t) and s not in psizes:
            psizes.append(s)
    if psizes:
        print()
        print("PAIRED BELOW-WALL REGRESSION  (flat, sharded, flat, sharded ... in one run)")
        print("  %-10s %-26s %8s %8s %8s %8s" %
              ("size", "pair", "flat ns", "shard ns", "median", "min-max"))
        for s in psizes:
            for a, b, what in (("p_ld_flat", "p_ld_divmod", "load, slow path"),
                               ("p_ld_flat", "p_ld_s0fast", "load, SHARD-0 FAST"),
                               ("p_ld_gflat", "p_ld_gshard", "load, guarded"),
                               ("p_st_flat", "p_st_divmod", "store, slow path"),
                               ("p_st_flat", "p_st_s0fast", "store, SHARD-0 FAST"),
                               ("a_ld_flat", "a_ld_divmod", "spread, slow path"),
                               ("a_ld_flat", "a_ld_s0fast", "spread, shard-0 fast")):
                ratios, fs, ds = [], [], []
                j = 1
                while ("%s.%d/%s" % (a, j, s)) in t:
                    f = t["%s.%d/%s" % (a, j, s)]["ns"]
                    d = t["%s.%d/%s" % (b, j, s)]["ns"]
                    ratios.append(d / f)
                    fs.append(f)
                    ds.append(d)
                    j += 1
                if not ratios:
                    continue
                ratios.sort()
                print("  %-10s %-26s %8.1f %8.1f %7.2fx  %.2f-%.2fx" %
                      (s, what, sorted(fs)[len(fs) // 2], sorted(ds)[len(ds) // 2],
                       ratios[len(ratios) // 2], ratios[0], ratios[-1]))

    print()
    print("THE TWO BARS")
    for s in sizes:
        f, d = t.get("ld_flat/" + s), t.get("ld_divmod/" + s)
        gf, gs = t.get("ld_guard_flat/" + s), t.get("ld_guard_shard/" + s)
        bf, bs = t.get("build_flat/" + s), t.get("build_shard/" + s)
        if not (f and d):
            continue
        print("  %-8s  slow-path load %.2fx | guarded load %.2fx | rebuild %.2fx"
              % (s, d["ns"] / f["ns"],
                 (gs["ns"] / gf["ns"]) if (gf and gs) else float("nan"),
                 (bs["ms"] / bf["ms"]) if (bf and bs) else float("nan")))
    print()


if __name__ == "__main__":
    main()
