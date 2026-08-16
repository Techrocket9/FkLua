#!/usr/bin/env python3
"""What each arm does to the churn guest's heap, and whether it computes the
same answer while doing it.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

This is the table agents/gc.md's premise rests on, re-taken for stage B with
the real allocator in it:

    the guest which reaches 16 MiB under -gc=leaking never leaves 0.125 MiB
    under a collector -- 128x, checksum-identical, measured through the real
    pipeline

`linear_words` is the number that matters and it is not the same as "bytes the
guest is using". Linear memory NEVER SHRINKS -- wasm has no memory.shrink,
MEMSIZE is authoritative on the Lua side, and Factorio walks the whole word
table in one indivisible propagatemark at 0.2 ms per MiB whether or not the
guest has anything in it.

    python3 scratchpad/gc/heapshape.py [--events 5000] [--batch 500]
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gclib  # noqa: E402


def body(events, batch, collect):
    """Run `events` events, collecting every `batch` of them when collect."""
    b = "local sum = 0\n"
    if collect and batch > 0:
        b += (f"local done = 0\n"
              f"while done < {events} do\n"
              f"  local n = {batch} if {events} - done < n then n = {events} - done end\n"
              f"  sum = K['churn_events'](n)\n"
              f"  done = done + n\n"
              f"  K['churn_collect']()\n"
              f"end\n")
    else:
        b += f"sum = K['churn_events']({events})\n"
    b += ("local st = K['churn_gc_stat']\n"
          "print(string.format('checksum=%d linear_words=%d bump=%d', "
          "sum, WORDS(), K['churn_heap_top']()))\n"
          "if st then print(string.format("
          "'  gc heap=%d live=%d free=%d cycles=%d grows=%d', "
          "st(0), st(1), st(2), st(3), st(4))) end\n")
    return b


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--events", type=int, default=5000)
    ap.add_argument("--batch", type=int, default=500)
    ap.add_argument("--arms", default="leaking,bump,fkgc,conservative")
    a = ap.parse_args()

    guest = os.path.join(gclib.ROOT, "guest", "go")
    print(f"# churn, {a.events} events, -opt=3 --persist=table, bin/lua52f")
    print(f"# 'collected' arms call churn_collect every {a.batch} events, from "
          f"OUTSIDE the guest call")
    for arm in a.arms.split(","):
        chunk = gclib.build(guest, "./examples/churn", "churn", arm)
        for collect in (False, True):
            if collect and arm not in ("fkgc",):
                continue
            tag = f"{arm}+collect" if collect else arm
            s = gclib.write(os.path.join(gclib.TMP, f"shape-{tag}.lua"),
                            gclib.head(chunk) + body(a.events, a.batch, collect))
            dt, out = gclib.run_once(s)
            print(f"\n{tag:>18}  ({dt*1000:.0f} ms wall)")
            for line in out.splitlines():
                print(f"{'':>18}  {line}")


if __name__ == "__main__":
    main()
