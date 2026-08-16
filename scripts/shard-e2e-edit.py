#!/usr/bin/env python3
"""Rewrite an emitted fk_module.lua's two access sites into the CONTROL arm.

STAGE A PROTOTYPED THE SHARDED ARMS BY HAND; STAGE B SHIPPED THEM. So this
script's job has inverted: the emitter now prints the `fast` arm itself, and
what is left to synthesise is the CONTROL -- `slow`, the arm in which every
access takes the runtime shard select unconditionally instead of merging it
into the bounds check.

That arm is not a straw man and it is why this file survives. agents/sharding.md
section 12: "any implementation that emits the shard select IN ADDITION TO the
bounds check has thrown the result away -- it measures 1.46-1.59x". `slow` is
that implementation, built from the same guest, the same runtime and the same
packaging, so the difference between `slow` and `fast` is the merge and nothing
else. If a future emitter change quietly un-merges the two tests, this arm and
`fast` converge and the harness says so.

The `flat` arm is a HISTORICAL baseline now: the emitter cannot print it any
more, so run-shard-e2e.sh builds it with a pre-sharding compiler named by
FLAT_FKLUA, or skips it.

It rewrites ONE known guest (testdata/shardprobe/e2e.wat.tmpl); every
replacement asserts it matched exactly once, so an emitter change breaks this
loudly rather than silently measuring two copies of the same file.
"""
import sys

SHW = 524288          # words per shard, 2^19
SHB = SHW * 4         # bytes per shard, 2 MiB


def sub(src, old, new, what):
    if src.count(old) != 1:
        sys.exit("shard-e2e-edit: %s matched %d times, want 1.\nwanted:\n%s"
                 % (what, src.count(old), old))
    return src.replace(old, new)


def main():
    path, arm = sys.argv[1], sys.argv[2]
    if arm != "slow":
        sys.exit("shard-e2e-edit: only the `slow` control arm is synthesised now; "
                 "`fast` is what the emitter prints and `flat` needs FLAT_FKLUA")
    s = open(path, encoding="utf-8").read()

    # 1. Scratch. The store's control form needs a third register to hold the
    #    within-shard offset, and Invariant B admits no `local` after the first
    #    ::label::, so it joins the prologue run.
    s = sub(s, "  local t0, t1\n", "  local t0, t1, t2\n", "the scratch declaration")

    # 2. Loop A's LOAD. The emitter proved this address 4-aligned, so it takes
    #    the no-else form: the slow arm rewrites t0 into a within-shard offset
    #    and t1 into that shard, and the tail is shared. The control replaces
    #    the whole thing with the bounds check AND an unconditional select.
    s = sub(s,
            "  t1 = S1\n"
            "  if t0 < 0 or t0 + 4 > SHBOUND then\n"
            "    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end\n"
            "    t1 = t0 %% %d\n"
            "    t1, t0 = MEM[(t0 - t1) / %d + 1], t1\n"
            "  end\n"
            "  v5 = t1[t0 / 4 + 1]\n" % (SHB, SHB),
            "  if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end\n"
            "  t1 = t0 %% %d\n"
            "  v5 = MEM[(t0 - t1) / %d + 1][t1 / 4 + 1]\n" % (SHB, SHB),
            "loop A's load")

    # 3. Loop B's STORE. Its addresses walk the WHOLE memory at an 8 KiB
    #    stride, so under `fast` it misses shard 0 on most iterations -- which
    #    is the point of having it: a guest whose working set is not at the
    #    bottom of its memory. The mark is preserved verbatim and stays after
    #    the bounds check, exactly as the emitter orders it.
    s = sub(s,
            "    if t0 >= 0 and t0 + 4 <= SHBOUND then\n"
            "      if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then "
            "MEMPACK.mark(t0, t0 + 3) end\n"
            "      S1[t0 / 4 + 1] = t1 %% 4294967296.0\n"
            "    else\n"
            "      if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end\n"
            "      if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then "
            "MEMPACK.mark(t0, t0 + 3) end\n"
            "      if t0 %% 4 == 0 then MEM[(t0 - t0 %% %d) / %d + 1]"
            "[t0 %% %d / 4 + 1] = t1 %% 4294967296.0 "
            "else st32(MEM, MEMSIZE, t0, t1) end\n"
            "    end\n" % (SHB, SHB, SHB),
            "    if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end\n"
            "    if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then "
            "MEMPACK.mark(t0, t0 + 3) end\n"
            "    t2 = t0 %% %d\n"
            "    MEM[(t0 - t2) / %d + 1][t2 / 4 + 1] = t1 %% 4294967296.0\n" % (SHB, SHB),
            "loop B's store")

    open(path, "w", encoding="utf-8").write(s)
    print("shard-e2e-edit: %s rewritten for arm=%s (shard = %d words)"
          % (path, arm, SHW))


if __name__ == "__main__":
    main()
