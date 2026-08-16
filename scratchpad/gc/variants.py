#!/usr/bin/env python3
"""Barrier-candidate variants, produced by rewriting an emitted chunk.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE. Nothing here is compiled,
imported or tested by the repo's gates; it exists so stage A could put a number
on each write-barrier candidate without building any of them into the emitter.
The precedent is the five ideas in CLAUDE.md's M12 row that were "prototyped by
hand-editing real emitted Lua and timed" -- same instrument, done by regex so it
is reproducible.

Every transform is checksum-gated by bench.py: a variant that computes a
different answer is not a variant.

The variants, keyed to the sketch in agents/gc.md:

  base        the chunk as `fklua compile --opt=3 --persist=table` emits it.
  pageset     (a)/(c) -- MEMDIRTY armed, i.e. packed's page-set maintenance on
              in table mode. One line changed; the marking is the runtime's own.
  flagstore   (b1) -- a second flag-checked branch in EVERY inlined store,
              including the guarded arm the loop guard emptied. `GCM` is false,
              so this is the idle cost of a per-store barrier check.
  flagunguard (b1') -- the same check, but only where the existing MEMDIRTY test
              already sits. Measures how much of b1 is the guarded arm.
  callstore   (b2, partial) -- unguarded inlined i32 stores go back to `st32`.
              Guarded arms are left alone: see agents/gc.md for why a full b2
              also surrenders those and why that makes b2 strictly worse than
              (c) rather than a different point on the same curve.
"""

import re

# --- (a)/(c): arm the page set -------------------------------------------
# MEMPACK.arm() also calls dirty_clear(), which only resets state that is
# already at its initial value in a fresh chunk, so flipping the declaration is
# equivalent and needs no hook into the persist surface (table mode does not
# export `arm`).
_MEMDIRTY_DECL = "local MEMDIRTY = false"


def pageset(src: str) -> str:
    out, n = re.subn(re.escape(_MEMDIRTY_DECL), "local MEMDIRTY = true", src, count=1)
    assert n == 1, "MEMDIRTY declaration not found"
    return out


# --- the barrier flag and its mark function ------------------------------
# GCM is ONE chunk local. The mark itself hangs off MEMPACK, for the reason
# fk_rt.lua gives for MEMPACK.mark: a column-zero name is the scarcest thing a
# generated chunk has, and TestPromotionLeavesTheMarginItPromises says one more
# breaks a 32-global guest at -opt=3 while it still compiles at -opt=2.
_FLAG_DECL = """local GCM = false
MEMPACK.gcmark = function(a) end
"""


_MEMPACK_DECL = "local MEMPACK = {}"


def _with_flag(src: str) -> str:
    # After MEMPACK's forward declaration, which is where the store leaves can
    # already see it.
    out, n = re.subn(re.escape(_MEMPACK_DECL), _MEMPACK_DECL + "\n" + _FLAG_DECL, src, count=1)
    assert n == 1
    return out


# The unguarded inlined-store barrier line, verbatim from emitInlineStore32.
_MARK32 = re.compile(
    r"^(\s*)if MEMDIRTY and \(t0 < DPLO or t0 \+ 3 > DPHI\) then MEMPACK\.mark\(t0, t0 \+ 3\) end$",
    re.M,
)

# The guarded arm: `if gNN then MEM[wNN_k] = <value> else`  (a store; the load
# form has the assignment on the other side and is not matched).
_GUARDED_ST = re.compile(r"^(\s*)if (g\d+) then MEM\[(w\d+_\d+(?: \+ \d+)?)\] = ", re.M)


def flagstore(src: str) -> str:
    src = _with_flag(src)
    src, n1 = _MARK32.subn(
        r"\1if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end\n"
        r"\1if GCM then MEMPACK.gcmark(t0) end",
        src,
    )
    src, n2 = _GUARDED_ST.subn(r"\1if \2 then if GCM then MEMPACK.gcmark(\3) end MEM[\3] = ", src)
    # n2 == 0 is legal and is itself a finding: a guest with no loop the guard
    # recognises has no guarded store arm to pay for.
    assert n1 > 0, f"flagstore matched {n1} unguarded, {n2} guarded"
    return src


def flagunguard(src: str) -> str:
    src = _with_flag(src)
    src, n1 = _MARK32.subn(
        r"\1if MEMDIRTY and (t0 < DPLO or t0 + 3 > DPHI) then MEMPACK.mark(t0, t0 + 3) end\n"
        r"\1if GCM then MEMPACK.gcmark(t0) end",
        src,
    )
    assert n1 > 0
    return src


# --- (b2, partial): un-inline the unguarded i32 store --------------------
# The emitted shape is exactly four lines, in this order:
#     t0 = <addr>
#     [t1 = <value>]                          -- only for a composite value
#     if t0 < 0 or t0 + 4 > MEMSIZE then trap_oob() end
#     if MEMDIRTY and (...) then MEMPACK.mark(t0, t0 + 3) end
#     if t0 % 4 == 0 then MEM[t0 / 4 + 1] = V else st32(MEM, MEMSIZE, t0, W) end
# The last line names both the aligned value V and the call's value W; W is what
# st32 takes, so the collapse keeps the call verbatim and drops the other three
# lines. The address assignment stays: t0 is what the call reads.
_INLINE32 = re.compile(
    r"^(\s*)if t0 < 0 or t0 \+ 4 > MEMSIZE then trap_oob\(\) end\n"
    r"\s*if MEMDIRTY and \(t0 < DPLO or t0 \+ 3 > DPHI\) then MEMPACK\.mark\(t0, t0 \+ 3\) end\n"
    r"\s*if t0 % 4 == 0 then MEM\[t0 / 4 \+ 1\] = .*? else (st32\(MEM, MEMSIZE, t0, .*?\)) end$",
    re.M,
)


def callstore(src: str) -> str:
    out, n = _INLINE32.subn(r"\1\2", src)
    assert n > 0, "no inlined i32 store matched"
    return out


# --- (c'): the same page set, with the cache widened -----------------------
# attribute.py says the armed set's cost is NOT the two-compare fast path, it
# is the rate at which a store LEAVES the one cached page and pays a
# MEMPACK.mark call. Two independent ways to cut that rate, both measured:
#
#   card64k   the marked unit becomes 64 KiB rather than 4 KiB. Legitimate for
#             a GC card table, whose card size is the collector's own choice;
#             NOT legitimate for packed's flush, where the page is the repack
#             granularity. This is the fork in candidate (c).
#   pageset2  a two-entry cache, so a store alternating between two pages stops
#             thrashing. Costs two more chunk locals, which is not free -- see
#             TestPromotionLeavesTheMarginItPromises.

_MARK_BODY = """    local p = (a - a % PAGEB) / PAGEB
    local q = (b - b % PAGEB) / PAGEB"""
_MARK_TAIL = """    DPLO = q * PAGEB
    DPHI = DPLO + PAGEB - 1"""


def card64k(src: str) -> str:
    src = pageset(src)
    out, n = re.subn(
        re.escape(_MARK_BODY),
        "    local CARDB = 65536\n"
        "    local p = (a - a % CARDB) / CARDB\n"
        "    local q = (b - b % CARDB) / CARDB",
        src,
        count=1,
    )
    assert n == 1
    out, n = re.subn(
        re.escape(_MARK_TAIL),
        "    DPLO = q * 65536\n    DPHI = DPLO + 65535",
        out,
        count=1,
    )
    assert n == 1
    return out


_DP_DECL = "local DPLO, DPHI = math.huge, -1"


def pageset2(src: str) -> str:
    src = pageset(src)
    out, n = re.subn(
        re.escape(_DP_DECL),
        "local DPLO, DPHI = math.huge, -1\nlocal DPLO2, DPHI2 = math.huge, -1",
        src,
        count=1,
    )
    assert n == 1
    # Rotate on every mark, so the two most recently marked pages are cached.
    out, n = re.subn(
        re.escape(_MARK_TAIL),
        "    DPLO2, DPHI2 = DPLO, DPHI\n    DPLO = q * PAGEB\n    DPHI = DPLO + PAGEB - 1",
        out,
        count=1,
    )
    assert n == 1
    # Every `X < DPLO or Y > DPHI` test gains the second page, short-circuited
    # so the hit path is still two compares.
    out, n2 = re.subn(
        r"\(([A-Za-z0-9_]+) < DPLO or ([^)]+) > DPHI\)",
        r"(\1 < DPLO or \2 > DPHI) and (\1 < DPLO2 or \2 > DPHI2)",
        out,
    )
    # 7 in the prelude (st8b, st16, st32, st64, fk_wstr, mem_copy, mem_fill),
    # plus one per inlined i32 store. A kernel with no memory has exactly 7.
    assert n2 >= 7, n2
    # dirty_clear and flush reset the cache; the second entry resets with it.
    out = out.replace("DPLO, DPHI = math.huge, -1\n", "DPLO, DPHI = math.huge, -1\n    DPLO2, DPHI2 = math.huge, -1\n")
    return out


VARIANTS = {
    "base": lambda s: s,
    "pageset": pageset,
    "card64k": card64k,
    "pageset2": pageset2,
    "flagstore": flagstore,
    "flagunguard": flagunguard,
    "callstore": callstore,
}


def counts(src: str) -> dict:
    """What each transform would touch -- printed by bench.py as a receipt."""
    return {
        "inlined_i32_store": len(_MARK32.findall(src)),
        "guarded_store_arm": len(_GUARDED_ST.findall(src)),
        "uninlinable_i32_store": len(_INLINE32.findall(src)),
    }


if __name__ == "__main__":
    import sys

    src = open(sys.argv[1]).read()
    print(counts(src))
    for name, fn in VARIANTS.items():
        out = fn(src)
        print(f"{name:12s} {len(out):8d} bytes  {out.count(chr(10)):6d} lines")
