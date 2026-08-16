#!/usr/bin/env python3
"""What one stop-the-world collection costs, against heap size.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

These are the numbers stage C has to beat. agents/gc.md derives the reference
implementation's pause the same way and gets ~10 ms for a 63 KB heap:

    the guest allocates 10.08 MB over 5,000 events, so it collects roughly 155
    times, and 1595 ms of extra time over 155 collections is about 10 ms per
    collection of a 63 KB heap -- most of a frame, landing at an arbitrary
    point inside an event handler, in a lockstep game. That is exactly the
    pause the incremental work exists to break up.

bin/lua52f is Factorio's sandbox, so there is no clock inside the guest to read
(agents/sandbox.md). A pause is therefore DERIVED: the same run with K
collections and with one, differenced and divided. That is the same instrument
agents/gc.md used, and it has the property that everything not a collection --
process start, chunk parse, building the live set -- cancels.

The heap is held FIXED across the K collections, so every one of them does the
same work: mark the same live set, sweep the same spans. What is being reported
is the cost of a collection at that heap size, not an average over a run.

    python3 scratchpad/gc/pause.py [--nodes 1000,5000,20000,80000] [--samples 7]
"""

import argparse
import os
import statistics
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gclib  # noqa: E402


def body(nodes, collections):
    return f"""
local st = K['torture_stat']
K['torture_build']({nodes})
K['torture_interior'](1)
K['torture_large'](40000)
for _ = 1, {collections} do K['torture_collect']() end
print(string.format('heap=%d live=%d liveobj=%d free=%d words=%d verify=%d',
  st(0), st(1), st(5), st(2), WORDS(), K['torture_verify']()))
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--nodes", default="1000,5000,20000,80000")
    ap.add_argument("--samples", type=int, default=7)
    ap.add_argument("--collections", type=int, default=40)
    a = ap.parse_args()

    guest = os.path.join(gclib.ROOT, "guest", "go")
    chunk = gclib.build(guest, "./examples/gctorture", "gctorture", "fkgc")

    print(f"# one stop-the-world mark and sweep, derived from {a.collections} "
          f"collections against 1, {a.samples} interleaved samples")
    print(f"\n  {'live set':>10} {'heap KiB':>9} {'live KiB':>9} {'objects':>9} "
          f"{'pause ms':>9} {'ms/MiB heap':>12} {'MiB/s':>8}")
    print("  " + "-" * 72)
    for nodes in [int(x) for x in a.nodes.split(",")]:
        cells = {}
        for tag, k in (("one", 1), ("many", a.collections)):
            cells[tag] = gclib.write(os.path.join(gclib.TMP, f"pause-{nodes}-{tag}.lua"),
                                     gclib.head(chunk) + body(nodes, k))
        raw, out = gclib.paired(cells, a.samples)
        one = statistics.median(raw["one"])
        many = statistics.median(raw["many"])
        per = (many - one) / (a.collections - 1)
        f = {}
        for kv in out["one"].split():
            k, _, v = kv.partition("=")
            f[k] = int(v)
        heap_mib = f["heap"] / (1024 * 1024)
        print(f"  {nodes:>10} {f['heap']/1024:>9.0f} {f['live']/1024:>9.0f} "
              f"{f['liveobj']:>9} {per*1000:>9.3f} "
              f"{(per*1000/heap_mib if heap_mib else 0):>12.2f} "
              f"{(heap_mib/per if per > 0 else 0):>8.1f}")
        sys.stdout.flush()

    print("\n  'MiB/s' is heap swept per second of collector time. A stage-C step")
    print("  budget of 0.5 ms/tick at 60 UPS is 3% duty, so sustained throughput")
    print("  is that column times 0.03.")


if __name__ == "__main__":
    main()
