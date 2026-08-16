#!/usr/bin/env python3
"""Characterise Lua's own collector tail from scripts/run-gctail.sh logs.

What it answers, in the order agents/guests.md needs it:

  1. The DISTRIBUTION of luaGarbageIncremental per tick, per arm -- mean,
     median, p90, p99, p999 and worst. A tail is not a mean and the cost table
     currently quotes only the extremes of two runs.
  2. Whether the tail scales with BYTES or with TABLE COUNT. The s* arms move
     both together; t52 and q52 hold the bytes fixed and multiply the tables.
     A per-table `propagatemark` bound must fall across t52/q52; Lua's atomic
     step, which is not incremental at all, must not.
  3. Whether it is PERIODIC. Every tick over a threshold is listed with the gap
     since the previous one, and the gaps are summarised. A collector cycle has
     a period; a scheduling artefact does not.
  4. What `collectgarbage` pacing does to it (p26 against s26).

HEADER-DRIVEN COLUMN PARSING, never positional: Factorio emits the counters in
its own canonical order rather than the order the command line asked for, so
counting columns silently relabels them the moment a counter is added. This is
the rule agents/benchmarks.md records, and it is why this file re-reads the
`tick,` header line rather than trusting COUNTERS.
"""
import argparse
import re
import sys

SETUP = re.compile(
    r"FKGCTAIL_SETUP shards=(?P<shards>\d+) shardw=(?P<shardw>\d+) "
    r"words=(?P<words>\d+) mib=(?P<mib>[\d.]+)")


def read(path, want, skip):
    """Per-tick values of one counter, in nanoseconds, load ticks dropped.

    Each --benchmark-runs pass restarts the tick numbering at t0, so `t < skip`
    drops the load tick of EVERY run rather than only the first one.
    """
    col, out, setup = None, [], None
    for line in open(path, encoding="utf-8", errors="replace"):
        m = SETUP.search(line)
        if m and setup is None:
            setup = m.groupdict()
            continue
        if line.startswith("tick,"):
            names = line.rstrip("\n").split(",")
            col = names.index(want) if want in names else None
            continue
        if col is None or not line.startswith("t"):
            continue
        f = line.rstrip("\n").split(",")
        if not f[0][1:].isdigit():
            continue
        t = int(f[0][1:])
        if t < skip or col >= len(f):
            continue
        try:
            out.append((t, float(f[col])))
        except ValueError:
            pass
    return setup, out


def pct(sorted_vals, q):
    if not sorted_vals:
        return float("nan")
    i = int(len(sorted_vals) * q)
    return sorted_vals[min(i, len(sorted_vals) - 1)]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--skip", type=int, default=1)
    ap.add_argument("--counter", default="luaGarbageIncremental")
    ap.add_argument("arms", nargs="+", help="name=path")
    a = ap.parse_args()

    arms = []
    for spec in a.arms:
        name, _, path = spec.partition("=")
        setup, rows = read(path, a.counter, a.skip)
        if not rows:
            print("  %-5s NO PER-TICK ROWS in %s -- --benchmark-verbose printed "
                  "no CSV for %s" % (name, path, a.counter), file=sys.stderr)
            continue
        arms.append((name, setup or {}, rows))
    if not arms:
        sys.exit("no arm produced per-tick rows")

    print("THE DISTRIBUTION  (%s per tick, ms; load ticks t<%d dropped)"
          % (a.counter, a.skip))
    print("  %-5s %7s %7s %8s %9s %8s %8s %8s %8s %8s" %
          ("arm", "MiB", "tables", "ticks", "mean", "median", "p90", "p99",
           "p99.9", "WORST"))
    for name, setup, rows in arms:
        v = sorted(x for _, x in rows)
        n = len(v)
        print("  %-5s %7s %7s %8d %9.4f %8.4f %8.4f %8.4f %8.4f %8.4f" %
              (name, setup.get("mib", "?"), setup.get("shards", "?"), n,
               sum(v) / n / 1e6, pct(v, .50) / 1e6, pct(v, .90) / 1e6,
               pct(v, .99) / 1e6, pct(v, .999) / 1e6, v[-1] / 1e6))

    # THE TAIL, tick by tick. A threshold rather than a top-N, because "how many
    # ticks are over a frame's worth" is the question a mod author has and a
    # top-10 answers it only by accident.
    print()
    print("THE TAIL  (every tick over 4x that arm's p99, with the gap since the last one)")
    print("  %-5s %9s %8s %10s %10s %9s" %
          ("arm", "over ms", "count", "first t", "median gap", "gap sd"))
    for name, setup, rows in arms:
        v = sorted(x for _, x in rows)
        bar = pct(v, .99) * 4
        hits = [t for t, x in rows if x >= bar]
        if not hits:
            print("  %-5s %9.4f %8d %10s %10s %9s" %
                  (name, bar / 1e6, 0, "-", "-", "-"))
            continue
        gaps = [b - c for c, b in zip(hits, hits[1:])]
        if gaps:
            g = sorted(gaps)
            med = g[len(g) // 2]
            mean = sum(gaps) / len(gaps)
            sd = (sum((x - mean) ** 2 for x in gaps) / len(gaps)) ** 0.5
            print("  %-5s %9.4f %8d %10d %10.1f %9.1f" %
                  (name, bar / 1e6, len(hits), hits[0], med, sd))
        else:
            print("  %-5s %9.4f %8d %10d %10s %9s" %
                  (name, bar / 1e6, len(hits), hits[0], "-", "-"))
    print("  (a gap standard deviation SMALL against its median is a PERIOD --")
    print("   i.e. a collector cycle. One comparable to the median is not.)")

    # THE TEN WORST TICKS of every arm, with their tick numbers, because a
    # single 4.178 ms outlier in a 24,000-tick run is exactly the shape that
    # started this and a distribution alone cannot say whether it recurs.
    print()
    print("THE TEN WORST TICKS  (tick number : ms)")
    for name, setup, rows in arms:
        top = sorted(rows, key=lambda r: -r[1])[:10]
        print("  %-5s %s" % (name, "  ".join("t%d:%.3f" % (t, x / 1e6) for t, x in top)))

    # WHAT IT SCALES WITH. Printed as ratios against the first arm so the reader
    # does not have to divide, and with BOTH candidate denominators, because
    # deciding between them is the whole question.
    print()
    print("WHAT THE TAIL SCALES WITH  (ratios against %s)" % arms[0][0])
    b_name, b_setup, b_rows = arms[0]
    bv = sorted(x for _, x in b_rows)
    b_worst, b_p99 = bv[-1], pct(bv, .99)
    b_mib = float(b_setup.get("mib", "nan"))
    b_tab = float(b_setup.get("shards", "nan"))
    print("  %-5s %8s %8s %10s %10s %10s" %
          ("arm", "MiB x", "tables x", "worst x", "p99 x", "mean x"))
    b_mean = sum(bv) / len(bv)
    for name, setup, rows in arms:
        v = sorted(x for _, x in rows)
        mib = float(setup.get("mib", "nan"))
        tab = float(setup.get("shards", "nan"))
        print("  %-5s %8.2f %8.2f %10.2f %10.2f %10.2f" %
              (name, mib / b_mib, tab / b_tab, v[-1] / b_worst,
               pct(v, .99) / b_p99, (sum(v) / len(v)) / b_mean))
    print("  (t52/q52 hold the MiB fixed and multiply the tables. If the worst")
    print("   tick is one table's propagatemark it must FALL down that column;")
    print("   if it is Lua's atomic step it cannot, because the atomic step")
    print("   walks the whole live set inside one indivisible tick.)")
    print()


if __name__ == "__main__":
    main()
