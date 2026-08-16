#!/usr/bin/env python3
"""Turn a probe run into committed baselines.

Reads testdata/probe/results/{probe.json,timings.txt} and writes
bench/baselines/probe-<game_version>.json plus a readable summary.

Timings arrive as log lines because helpers.create_profiler() is the only clock
in Factorio's sandbox and it refuses to hand Lua a raw number -- a LuaProfiler
can only be rendered into a LocalisedString, so the harness scrapes the log.

This lives in Python because M0 is a spike; it moves into `fklua bench` at M1.
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
RESULTS = ROOT / "testdata" / "probe" / "results"

TIME_RE = re.compile(
    r"FKPROBE_TIME\t(?P<label>[^\t]+)\t(?P<reps>\d+)\t.*?Duration:\s*(?P<ms>[\d.]+)ms"
)


def load_timings():
    out = {}
    for line in (RESULTS / "timings.txt").read_text().splitlines():
        m = TIME_RE.search(line)
        if m:
            reps = int(m.group("reps"))
            ms = float(m.group("ms"))
            out[m.group("label")] = {
                "reps": reps,
                "total_ms": ms,
                "ns_per_op": ms * 1e6 / reps,
            }
    return out


def main():
    probe = json.loads((RESULTS / "probe.json").read_text())
    t = load_timings()
    if not t:
        sys.exit("no FKPROBE_TIME lines found in timings.txt")

    # The empty loop is the floor for everything measured inside one. Subtracting
    # it turns "how long did the loop take" into "what does the operation cost".
    base = t["baseline_loop"]["ns_per_op"]
    # An unwrapped add costs the same as the empty loop, so use it as the floor
    # for the wrap variants specifically -- they all perform that add too.
    add_base = t.get("add_nowrap", t["baseline_loop"])["ns_per_op"]

    def marginal(label, floor=None):
        if label not in t:
            return None
        return round(t[label]["ns_per_op"] - (floor if floor is not None else base), 3)

    forks = {
        "i32.add wrap": {
            "% (overflowing)": marginal("add_wrap_mod_taken", add_base),
            "% (non-overflowing)": marginal("add_wrap_mod_nottaken", add_base),
            "conditional fixup (branch taken)": marginal("add_wrap_cond_taken", add_base),
            "conditional fixup (branch not taken)": marginal("add_wrap_cond_nottaken", add_base),
            "bit32.band": marginal("add_wrap_bit32", add_base),
        },
        "i32.shr_u": {
            "(a - a%2^n)/2^n": marginal("shr_modsub"),
            "math.floor(a/2^n)": marginal("shr_floor"),
            "bit32.rshift": marginal("shr_bit32"),
        },
        "i32.mul": {
            "by small constant": marginal("mul_const_small"),
            "16-bit split, magic floor": marginal("mul_split_magic"),
            "16-bit split, math.floor": marginal("mul_split_floor"),
            "16-bit split, bit32": marginal("mul_split_bit32"),
        },
        "linear memory read": {
            "array part, 1-based": round(t["mem_array_1based"]["ns_per_op"], 3),
            "array part, 0-based": round(t["mem_array_0based"]["ns_per_op"], 3),
            "hash part": round(t["mem_hash_part"]["ns_per_op"], 3),
        },
        "f64 bit-punning": {
            "math.frexp (exponent only)": marginal("pun_frexp"),
            "string.pack": marginal("pun_stringpack"),
        },
        "call dispatch": {
            "upvalue": marginal("call_upvalue"),
            "F[idx] table": marginal("call_table"),
        },
    }

    parse = {}
    for label, rec in t.items():
        if label.startswith("load_parse_"):
            parse[label] = {
                "bytes": rec["reps"],
                "ms": rec["total_ms"],
                "mb_per_s": round(rec["reps"] / rec["total_ms"] / 1000, 2),
            }

    storage = {
        k: {"entries": t[k]["reps"], "ms": t[k]["total_ms"]}
        for k in ("storage_build_table", "storage_pack_pages")
        if k in t
    }

    out = {
        "source": "in-game probe, Factorio " + str(probe["meta"].get("game_version")),
        "lua_version": probe["meta"].get("lua_version"),
        "loop_baseline_ns": round(base, 3),
        "add_baseline_ns": round(add_base, 3),
        "note": (
            "ns_per_op values are marginal: the empty-loop cost is subtracted. "
            "Measured on one machine in one run; use for RELATIVE comparison "
            "between lowerings, not as absolute performance targets."
        ),
        "codegen_forks": forks,
        "load_parse": parse,
        "storage": storage,
        "raw": {k: round(v["ns_per_op"], 4) for k, v in sorted(t.items())},
    }

    dest = ROOT / "bench" / "baselines" / f"probe-{probe['meta'].get('game_version')}.json"
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(json.dumps(out, indent=2) + "\n")

    print(f"loop baseline: {base:.3f} ns/iter   (unwrapped add: {add_base:.3f})\n")
    for group, opts in forks.items():
        ranked = sorted((v, k) for k, v in opts.items() if v is not None)
        best = ranked[0][0]
        print(f"{group}:")
        for v, k in ranked:
            ratio = f"{v / best:.2f}x" if best > 0 else "-"
            print(f"    {v:8.2f} ns  {ratio:>6}  {k}")
        print()
    print("load() parse throughput:")
    for k in sorted(parse, key=lambda x: parse[x]["bytes"]):
        p = parse[k]
        print(f"    {p['bytes']:>9,} B  {p['ms']:8.2f} ms  {p['mb_per_s']:6.1f} MB/s")
    print(f"\nwrote {dest.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
