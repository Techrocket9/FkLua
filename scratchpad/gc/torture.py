#!/usr/bin/env python3
"""The retention gate: does the collector keep what is live and drop what is not.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.

Two questions, and the second is the kill criterion.

    (1) Did everything reachable survive? A node the collector wrongly
        reclaimed does not vanish -- it is zeroed and handed to somebody else --
        so the symptom is a checksum, and the checksum is compared against the
        SAME guest built -gc=leaking, where nothing is reclaimed and the answer
        is right by construction.

    (2) How much did it keep that was NOT reachable? agents/gc.md risk 2 says
        the range test gets more permissive as the heap grows, so this is run
        at several live-set sizes and the big ones are the interesting ones.

    python3 scratchpad/gc/torture.py [--nodes 2000,20000,80000]
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gclib  # noqa: E402


def scenario(nodes):
    return f"""
local st = K['torture_stat']
local build = K['torture_build']({nodes})
local ipv   = K['torture_interior'](12345)
local opv   = K['torture_one_past'](777)
local bigv  = K['torture_large'](40000)
local before = K['torture_verify']()
K['torture_collect']()
print(string.format('build=%d verify_before=%d verify_after=%d', build, before, K['torture_verify']()))
print(string.format('interior=%d/%d large=%d/%d one_past_retained=%d',
  K['torture_interior_read'](), ipv, K['torture_large_read'](), bigv, K['torture_one_past_read']()))
print(string.format('kept_bytes=%d gc_live=%d gc_liveobj=%d heap=%d free=%d words=%d',
  K['torture_kept_bytes'](), st(1), st(5), st(0), st(2), WORDS()))
K['torture_drop_all']()
K['torture_collect']()
K['torture_collect']()
print(string.format('after_drop  gc_live=%d gc_liveobj=%d heap=%d free=%d cycles=%d grows=%d',
  st(1), st(5), st(0), st(2), st(3), st(4)))
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--nodes", default="2000,20000,80000")
    a = ap.parse_args()

    guest = os.path.join(gclib.ROOT, "guest", "go")
    chunks = {arm: gclib.build(guest, "./examples/gctorture", "gctorture", arm)
              for arm in ("leaking", "fkgc")}

    for nodes in [int(x) for x in a.nodes.split(",")]:
        print(f"\n=== {nodes} nodes ({nodes * 2} objects built, one root in eight)")
        outs = {}
        for arm, chunk in chunks.items():
            s = gclib.write(os.path.join(gclib.TMP, f"tort-{arm}-{nodes}.lua"),
                            gclib.head(chunk) + scenario(nodes))
            _, out = gclib.run_once(s)
            outs[arm] = out
            for line in out.splitlines():
                print(f"  {arm:>8}  {line}")
        # The differential: -gc=leaking reclaims nothing, so its checksums are
        # right by construction. Only the checksum lines have to agree; the
        # heap lines are the whole point of the difference.
        for tag in ("build=", "interior=",):
            la = [l for l in outs["leaking"].splitlines() if l.startswith(tag)]
            fa = [l for l in outs["fkgc"].splitlines() if l.startswith(tag)]
            print(f"  {'CHECK':>8}  {tag[:-1]:<10} "
                  f"{'AGREE' if la == fa else 'DIFFER -- ' + str((la, fa))}")


if __name__ == "__main__":
    main()
