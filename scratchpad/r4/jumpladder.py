#!/usr/bin/env python3
"""What Lua 5.2 ACTUALLY refuses, and whether a trampoline can relay a long jump.

THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE. It is round 4c's evidence.

internal/luagen/funclimit.go models Lua's refusal as the byte distance between
one `goto` and its `::label::`. That model is INCOMPLETE, and this is what says
so: a ladder of trampolines whose every hop is well under the limit is refused
anyway, because `::T:: goto X` with NOTHING BETWEEN THE LABEL AND THE GOTO
concatenates the two pending-jump lists and the whole ladder is patched as one
jump. One statement between them discharges the list and the hops become
independent -- at which point a ten-hop ladder spans 500,000 instructions,
3.8x Lua's single-jump limit, and loads.

  ./bin/lua52f -e 'x' ... -- run it with:  python3 scratchpad/r4/jumpladder.py | ./bin/lua52f -
"""

import sys


def body(n):
    return "\n".join("  v = v + 1" for _ in range(n))


def ladder(hops, chunk, sep, guarded=True, unreachable=False):
    """A chain T1 -> T2 -> ... -> L0, one hop per chunk of filler."""
    parts = []
    for i in range(hops):
        parts.append(body(chunk))
        nxt = "L0" if i == hops - 1 else f"T{i+2}"
        tramp = f"  ::T{i+1}::{sep} goto {nxt}"
        if unreachable:
            # Placed where nothing falls through, so the straight-line path pays
            # NOTHING -- not even the jump over the trampoline.
            parts.append(f"  if v < 0 then do return -2 end end\n"
                         f"  do goto S{i+1} end\n{tramp}\n  ::S{i+1}::")
        elif guarded:
            parts.append(f"  goto S{i+1}\n{tramp}\n  ::S{i+1}::")
        else:
            parts.append(tramp)
    return ("local v = 0\nif v ~= 0 then goto T1 end\n" + "\n".join(parts) +
            "\ndo return v end\n::L0::\nreturn -1")


def one(total):
    return f"local v = 0\ngoto L0\n{body(total)}\n::L0::\nreturn v"


def emit(label, src):
    print(f"do local f, e = load([==[\n{src}\n]==])\n"
          f"print(string.format('%-58s %s', [==[{label}]==], "
          f"f and ('LOADS, returns ' .. tostring(f())) or ('REFUSED -- ' .. tostring(e)))) end")


def main():
    # The limit itself, both sides, so the harness prices the machine it runs on.
    emit("one jump over 131071 instructions (Lua's stated limit)", one(131071))
    emit("one jump over 150000 instructions", one(150000))
    # The ladder, bare -- every hop far under the limit, and refused anyway.
    for hops, chunk in ((2, 50000), (4, 50000), (10, 50000)):
        emit(f"BARE ladder, {hops:2d} hops x {chunk} = {hops*chunk} total",
             ladder(hops, chunk, ""))
    # ...and with one statement between the label and the goto.
    for hops, chunk in ((4, 50000), (10, 50000)):
        emit(f"SEPARATED ladder, {hops:2d} hops x {chunk} = {hops*chunk} total",
             ladder(hops, chunk, " v = v + 0"))
    # ...placed where nothing falls through: zero cost on the straight-line path,
    # and the return value proves the body ran and no trampoline was entered.
    emit("UNREACHABLE-placed ladder, 10 hops x 50000 = 500000 total",
         ladder(10, 50000, " v = v + 0", unreachable=True))


if __name__ == "__main__":
    main()
