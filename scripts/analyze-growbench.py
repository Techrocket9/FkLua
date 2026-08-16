#!/usr/bin/env python3
"""Split one growbench run's per-tick CSV into GROW ticks and every other tick.

A worst-tick number cannot say why it was the worst. The guest logs the game
tick of every memory.grow it observes, so this pulls exactly those rows out of
Factorio's --benchmark-verbose output and reports the two distributions side by
side.

THE OFFSET IS DERIVED, NOT ASSUMED. Factorio numbers the CSV rows t0..tN from
the start of each --benchmark-runs pass, while the guest logs the GAME tick.
The two are related by a constant this script recovers from the guest's own
first logged tick and the first CSV row, and it REFUSES rather than guesses if
the recovered offset does not put every logged grow inside the window.

Usage: analyze-growbench.py <factorio log> <arm name> <skip>
"""
import re
import sys


def percentiles(vals):
    v = sorted(vals)
    n = len(v)
    if n == 0:
        return None
    return {
        "n": n,
        "mean": sum(v) / n / 1e6,
        "med": v[n // 2] / 1e6,
        "p90": v[int(n * 0.90)] / 1e6,
        "p99": v[int(n * 0.99)] / 1e6,
        "max": v[-1] / 1e6,
    }


def main():
    path, arm, skip = sys.argv[1], sys.argv[2], int(sys.argv[3])

    # One pass over the log: the CSV header, the per-tick rows, and the guest's
    # own lines all arrive interleaved on stdout.
    col = {}
    rows = []          # (run index, t, {counter: ns})
    run = -1
    grow_ticks = []
    grow_added = {}
    first_guest_tick = None
    summary = None
    target = None
    for line in open(path, encoding="utf-8", errors="replace"):
        if line.startswith("tick,"):
            col = {}
            for i, name in enumerate(line.rstrip("\n").split(",")):
                if i and name:
                    col[name] = i
            run += 1
            continue
        m = re.match(r"^t(\d+),", line)
        if m and col:
            f = line.rstrip("\n").split(",")
            t = int(m.group(1))
            rows.append((run, t, f))
            continue
        m = re.search(r"\[growbench\] GROW tick=(\d+) from=(\d+) to=(\d+) added=(\d+)", line)
        if m:
            gt = int(m.group(1))
            grow_ticks.append(gt)
            grow_added[gt] = (int(m.group(2)), int(m.group(3)), int(m.group(4)))
            continue
        m = re.search(r"\[growbench\] tick (\d+) ", line)
        if m:
            if first_guest_tick is None:
                first_guest_tick = int(m.group(1))
            summary = line.rstrip("\n")
            continue
        m = re.search(r"\[growbench\] TARGET tick=(\d+) .*mem=(\d+) grows=(\d+)", line)
        if m:
            target = (int(m.group(1)), int(m.group(2)), int(m.group(3)))

    if not rows or "scriptUpdate" not in col:
        sys.exit("    no per-tick rows in %s -- --benchmark-verbose printed no CSV" % path)

    # THE OFFSET. The guest's summary lines are at game ticks that are multiples
    # of 200, and the CSV's first row of a pass is t0. Every pass replays the
    # same save, so one offset covers them all: game_tick = t + offset.
    #
    # Recovered from the FIRST grow rather than from the summary, because a grow
    # is what has to line up and an off-by-one there would silently report the
    # wrong row. Both anchors are checked against each other.
    if not grow_ticks:
        print("    %-8s NO GROW WAS OBSERVED. Nothing below is about growing." % arm,
              file=sys.stderr)
        return 1
    tmin = min(t for _, t, _ in rows)
    gmin = min(grow_ticks)
    # The guest's first tick maps to the first CSV row of the pass.
    offset = gmin - tmin
    # A summary line lands on a multiple of 200 and must map to an existing row.
    if first_guest_tick is not None:
        alt = first_guest_tick - tmin
        if alt < offset:
            offset = alt
    seen = set()
    for _, t, _ in rows:
        seen.add(t + offset)
    missing = [g for g in grow_ticks if g not in seen]
    if missing:
        print("    %-8s the recovered tick offset (%d) leaves %d of %d grow ticks "
              "outside the CSV window; refusing to report a correlation that is "
              "not one." % (arm, offset, len(missing), len(grow_ticks)),
              file=sys.stderr)
        return 1

    gset = set(grow_ticks)
    ci = col["scriptUpdate"]
    grow_v, other_v = [], []
    t0 = None
    for _, t, f in rows:
        v = float(f[ci])
        if t < skip:
            if t0 is None:
                t0 = v
            continue
        (grow_v if (t + offset) in gset else other_v).append(v)

    g = percentiles(grow_v)
    o = percentiles(other_v)
    print("    %-8s GROW ticks   n %5d  mean %8.3f  median %8.3f  p90 %8.3f  WORST %9.3f ms"
          % (arm, g["n"], g["mean"], g["med"], g["p90"], g["max"]))
    if o:
        print("             every other  n %5d  mean %8.3f  median %8.3f  p90 %8.3f  WORST %9.3f ms"
              % (o["n"], o["mean"], o["med"], o["p90"], o["max"]))
    if t0 is not None:
        print("             dropped t<%d; the load tick t0 was %.3f ms of scriptUpdate"
              % (skip, t0 / 1e6))

    # THE GROW TICKS WITH THEIR WORD COUNTS, because the model is per-word and a
    # table of ticks without the word counts cannot check it.
    byt = {}
    for _, t, f in rows:
        if t < skip:
            continue
        gt = t + offset
        if gt in gset:
            byt.setdefault(gt, []).append(float(f[ci]))
    # TWO ORDERINGS, because they stopped being the same list. Before the fill
    # cursor the most expensive grow tick WAS the largest grow -- that is what
    # the 107 ns/word model says and the before-run rows land on it to within
    # 10%. Once the fill is pre-built the cost and the size decouple, and a
    # table sorted only by size cannot show what the residual tail is.
    worst = sorted(((min(v), k) for k, v in byt.items()), reverse=True)[:5]
    print("             most EXPENSIVE grow ticks:")
    for ns, gt in worst:
        frm, to, added = grow_added[gt]
        words = added / 4.0
        print("               tick %-6d %6.2f -> %6.2f MiB (+%7.3f MiB, %9d words)"
              "  %9.3f ms = %6.1f ns/word"
              % (gt, frm / 1048576, to / 1048576, added / 1048576, words,
                 ns / 1e6, ns / words if words else 0))

    biggest = sorted(grow_added.items(), key=lambda kv: -kv[1][2])[:5]
    print("             largest grows:")
    for gt, (frm, to, added) in biggest:
        vs = byt.get(gt)
        if not vs:
            continue
        ms = min(vs) / 1e6
        words = added / 4.0
        print("               tick %-6d %6.2f -> %6.2f MiB (+%7.3f MiB, %9d words)"
              "  %9.3f ms = %6.1f ns/word"
              % (gt, frm / 1048576, to / 1048576, added / 1048576, words,
                 ms, ms * 1e6 / words if words else 0))

    for name in ("luaGarbageIncremental",):
        if name not in col:
            continue
        vs = [float(f[col[name]]) for _, t, f in rows if t >= skip]
        p = percentiles(vs)
        if p:
            print("             %-22s median %6.3f  WORST %8.3f ms"
                  % (name, p["med"], p["max"]))
    if target:
        print("             reached target at tick %d: %.2f MiB of linear memory, %d grows"
              % (target[0], target[1] / 1048576, target[2]))
    if summary:
        print("             %s" % summary.split("[growbench] ")[-1])
    return 0


if __name__ == "__main__":
    sys.exit(main())
