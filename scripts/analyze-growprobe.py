#!/usr/bin/env python3
"""Turn a growprobe run into the grow cost model for agents/guests.md.

Reads the FKGROW lines scraped out of factorio-current.log by
scripts/run-growprobe.sh. Timings arrive as log lines because
helpers.create_profiler() is the only clock in Factorio's sandbox and it
refuses to hand Lua a raw number.

What it prints, in the order the design needs it:

  1. ns/word for a grow at each increment and heap size -- the 107 ns/word
     model, confirmed or replaced.
  2. The FIXED cost per grow call, solved from the two extreme increments at
     each size: t = fixed + words * per_word over the four increments, by
     least squares. This is the number that says how small an increment can
     usefully be.
  3. The total cost of reaching 40 MiB under each increment policy, which is
     what a smaller increment costs in aggregate.
  4. What a pre-build reduces the growing tick to (the splice rows).
"""
import re
import sys

TIME_RE = re.compile(
    r"FKGROW\t(?P<label>[^\t]+)\t(?P<words>\d+)\t.*?Duration:\s*(?P<ms>[\d.]+)\s*ms"
)

INCS = ["64KiB", "256KiB", "1MiB", "4MiB"]
INC_WORDS = {"64KiB": 16384, "256KiB": 65536, "1MiB": 262144, "4MiB": 1048576}
ATS = ["4", "16", "40"]


def linfit(xs, ys):
    """Least squares y = a + b*x. Returns (a, b)."""
    n = len(xs)
    if n < 2:
        return float("nan"), float("nan")
    sx = sum(xs)
    sy = sum(ys)
    sxx = sum(x * x for x in xs)
    sxy = sum(x * y for x, y in zip(xs, ys))
    den = n * sxx - sx * sx
    if den == 0:
        return float("nan"), float("nan")
    b = (n * sxy - sx * sy) / den
    a = (sy - b * sx) / n
    return a, b


def main():
    path = sys.argv[1]
    t = {}
    for line in open(path, encoding="utf-8", errors="replace"):
        m = TIME_RE.search(line)
        if not m:
            continue
        lab = m.group("label")
        rec = {"words": int(m.group("words")), "ms": float(m.group("ms"))}
        rec["ns"] = rec["ms"] * 1e6 / rec["words"] if rec["words"] else float("nan")
        # A repeated label keeps the FASTEST: --benchmark-runs > 1 replays the
        # whole probe and the quiet slice is the one to model against.
        if lab not in t or rec["ms"] < t[lab]["ms"]:
            t[lab] = rec
    if not t:
        sys.exit("no FKGROW timing lines found in " + path)

    print()
    print("ONE GROW, ns PER WORD  (grow = create the slot; fill = write a slot that exists)")
    print("  %-10s %-10s %12s %10s %12s %10s" %
          ("heap MiB", "increment", "grow ms", "ns/word", "fill ms", "ns/word"))
    for at in ATS:
        for inc in INCS:
            g = t.get("grow_%s@%s" % (inc, at))
            f = t.get("fill_%s@%s" % (inc, at))
            if not g:
                continue
            print("  %-10s %-10s %12.3f %10.1f %12s %10s" %
                  (at, inc, g["ms"], g["ns"],
                   "-" if not f else "%.3f" % f["ms"],
                   "-" if not f else "%.1f" % f["ns"]))

    print()
    print("THE MODEL  t(ms) = fixed + words x per_word, fitted over the four increments")
    print("  %-10s %14s %14s" % ("heap MiB", "fixed (ms)", "ns/word"))
    for at in ATS:
        xs, ys = [], []
        for inc in INCS:
            g = t.get("grow_%s@%s" % (inc, at))
            if g:
                xs.append(float(g["words"]))
                ys.append(g["ms"])
        a, b = linfit(xs, ys)
        print("  %-10s %14.4f %14.1f" % (at, a, b * 1e6))
    print("  (the FIXED term is what bounds how small an increment is worth making:")
    print("   an increment whose fill is below it is paying more overhead than work)")

    print()
    print("TOTAL COST OF AN INCREMENT POLICY  (0 -> 40 MiB, every word written once)")
    print("  %-10s %12s %12s %12s" % ("increment", "grows", "total ms", "vs 4MiB"))
    base = t.get("build_4MiB")
    for inc in INCS:
        b = t.get("build_%s" % inc)
        if not b:
            continue
        grows = 40 * 262144 // INC_WORDS[inc]
        print("  %-10s %12d %12.1f %12s" %
              (inc, grows, b["ms"],
               "-" if not base else "%.3fx" % (b["ms"] / base["ms"])))

    print()
    print("THE PRE-BUILD  (pre = the paced work, on some OTHER tick; splice = the growing tick)")
    print("  %-10s %-10s %12s %12s %10s" %
          ("heap MiB", "increment", "pre ms", "splice ms", "splice/grow"))
    for at in ATS:
        for inc in INCS:
            p = t.get("pre_%s@%s" % (inc, at))
            s = t.get("splice_%s@%s" % (inc, at))
            g = t.get("grow_%s@%s" % (inc, at))
            if not (p and s):
                continue
            print("  %-10s %-10s %12.3f %12.4f %10s" %
                  (at, inc, p["ms"], s["ms"],
                   "-" if not g else "%.4f" % (s["ms"] / g["ms"])))

    print()
    print("WHAT PACING THE PRE-BUILD ITSELF COSTS  (one 2 MiB shard, 8,192-word pieces)")
    print("  %-10s %12s %12s %10s" % ("heap MiB", "one shot ms", "paced ms", "overhead"))
    for at in ATS:
        o = t.get("oneshot_shard@%s" % at)
        p = t.get("paced_shard@%s" % at)
        if not (o and p):
            continue
        print("  %-10s %12.2f %12.2f %9.3fx" % (at, o["ms"], p["ms"], p["ms"] / o["ms"]))
    print()
    print("THE CLONE  ({table.unpack(z,1,524288)} against the same shard filled by loop)")
    print("  %-10s %12s %12s %10s" % ("heap MiB", "loop ms", "clone ms", "vs loop"))
    for at in ATS:
        o = t.get("oneshot_shard@%s" % at)
        c = t.get("clone_shard@%s" % at)
        if not (o and c):
            continue
        print("  %-10s %12.2f %12.2f %9.3fx" % (at, o["ms"], c["ms"], c["ms"] / o["ms"]))
    print()
    residual(t)


def pieces(t, arm, at):
    """Every logged piece of one arm at one heap size, in piece order."""
    pref = "%spiece@%s#" % (arm, at)
    got = [(int(k[len(pref):]), v) for k, v in t.items() if k.startswith(pref)]
    got.sort()
    return got


def residual(t):
    """Section 6: the three shapes for one 2 MiB shard.

    THE WORST PIECE IS WHAT CHOOSES, NOT THE TOTAL. Pacing already moved the
    total onto ticks nobody is waiting on, so a shape that is cheaper in
    aggregate and larger in its largest piece is a REGRESSION -- which is the
    opposite of how the presize was scored when agents/sharding.md section 15
    first refused it, because at that point the fill was not yet paced and the
    total WAS the worst tick.
    """
    if not any(k.startswith(("Apiece@", "Bpresize@", "Cpresize@")) for k in t):
        return
    print("THE SHARD-DOUBLING RESIDUAL  (one 2 MiB shard, three shapes)")
    print("  A = pace-plus-doubling (ships)   B = presize at creation"
          "   C = presize half, pace the rest")
    print("  %-6s %-28s %10s %10s %8s" %
          ("heap", "shape", "worst ms", "total ms", "pieces"))
    for at in ATS:
        rows = []
        ap = pieces(t, "A", at)
        if ap:
            ms = [v["ms"] for _, v in ap]
            rows.append(("A  paced fill, %d pieces" % len(ms),
                         max(ms), sum(ms), len(ms), None))
        for arm, width in (("B", "2^19"), ("C", "2^18")):
            pre = t.get("%spresize@%s" % (arm, at))
            if not pre:
                continue
            ms = [v["ms"] for _, v in pieces(t, arm, at)]
            # The clone is ONE indivisible tick. The pieces after it are what a
            # pre-build would still be asked to do over slots that now exist.
            rows.append(("%s  clone %s + %d pieces" % (arm, width, len(ms)),
                         max([pre["ms"]] + ms), pre["ms"] + sum(ms),
                         1 + len(ms), pre["ms"]))
        for name, worst, total, n, indiv in rows:
            note = "" if indiv is None else "   (%.1f ms of it INDIVISIBLE)" % indiv
            print("  %-6s %-28s %10.2f %10.2f %8d%s" %
                  (at, name, worst, total, n, note))
        if rows:
            print()

    # WHERE THE DOUBLING LANDS, which is the whole question arm C asks. If C's
    # worst piece is its FIRST one, the presize did not skip the doubling, it
    # moved it one piece later.
    print("  WHERE THE DOUBLING LANDS  (the piece whose cost is that arm's max)")
    print("  %-6s %-4s %8s %12s %10s %11s" %
          ("heap", "arm", "piece", "word offset", "worst ms", "median ms"))
    for at in ATS:
        for arm in ("A", "B", "C"):
            got = pieces(t, arm, at)
            if not got:
                continue
            ms = sorted(v["ms"] for _, v in got)
            i, rec = max(got, key=lambda kv: kv[1]["ms"])
            print("  %-6s %-4s %8d %12d %10.2f %11.3f" %
                  (at, arm, i, i * 8192, rec["ms"], ms[len(ms) // 2]))
    print()


if __name__ == "__main__":
    main()
