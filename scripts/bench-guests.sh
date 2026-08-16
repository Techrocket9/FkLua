#!/usr/bin/env bash
# The cross-language benchmark: real TinyGo and real Rust, through FkLua,
# against idiomatic hand-written Lua, on identical work.
#
# This answers a question neither existing benchmark can. `fklua bench` runs
# bench/kernels/, which is hand-written Lua MODELLING what the emitter produces
# -- an idealisation that pins a ceiling. `fklua bench --opt` runs bench/wasm/,
# hand-written .wat compiled by the real compiler -- which measures the passes
# but not a toolchain. Neither one runs TinyGo or rustc, so neither can say what
# a mod author actually gets: a real toolchain brings its own runtime, its own
# allocator, its own bounds checks and its own idea of a struct layout, and all
# of that lands in the Lua too.
#
# Every kernel returns a checksum and this script refuses to report a timing
# unless all three languages agree on it. A variant that computes a different
# answer is not a faster variant -- and that check has already caught three real
# bugs in these kernels, including a 32-bit hash that silently overflows a
# double in the Lua version.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/testdata/tmp/bench-guests"
LUA52F="$ROOT/bin/lua52f"
OPT="${OPT:-3}"
REPS="${REPS:-3}"
RUNS="${RUNS:-5}"

[ -x "$LUA52F" ] || { echo "bin/lua52f is missing; run: make lua52f" >&2; exit 1; }
command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
command -v wasm-opt >/dev/null || { echo "wasm-opt is not installed: brew install binaryen" >&2; exit 1; }
command -v cargo >/dev/null || { echo "cargo is not installed: https://rustup.rs" >&2; exit 1; }

mkdir -p "$OUT"
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"

echo "==> building the Go guest"
( cd "$ROOT/bench/guests/go" && tinygo build -target=wasm-unknown \
    -scheduler=none -gc=leaking -opt=2 -o "$OUT/k-go.wasm" . )

echo "==> building the Rust guest"
# rustc enables bulk-memory and friends by default for this target; the flags
# live in bench/guests/rust/.cargo/config.toml and wasm-opt lowers what
# compiler_builtins ships precompiled. See agents/guests.md.
( cd "$ROOT/bench/guests/rust"
  cargo build --release --target wasm32-unknown-unknown >/dev/null
  wasm-opt --llvm-memory-copy-fill-lowering --enable-bulk-memory -O3 \
    target/wasm32-unknown-unknown/release/benchkernels.wasm -o "$OUT/k-rs.wasm" )

echo "==> compiling both with fklua -opt=$OPT"
for g in go rs; do
  "$ROOT/bin/fklua" compile "$OUT/k-$g.wasm" --opt="$OPT" -o "$OUT/k-$g.lua" >/dev/null
done

echo "==> running"
OUT="$OUT" ROOT="$ROOT" LUA52F="$LUA52F" REPS="$REPS" RUNS="$RUNS" python3 - <<'PY'
import os, subprocess, time, sys

OUT, ROOT, LUA = os.environ["OUT"], os.environ["ROOT"], os.environ["LUA52F"]
REPS, RUNS = int(os.environ["REPS"]), int(os.environ["RUNS"])

# kernel -> (setup export, setup arg, kernel args, category, description)
KERNELS = [
    ("pure_sum",      "pure_setup", 65536, "40",       "pure", "u32 array reduction"),
    ("pure_prng",     None,         0,     "2000000",  "pure", "xorshift32, no memory at all"),
    ("pure_dot",      "dot_setup",  32768, "40",       "pure", "f64 dot product"),
    ("real_entities", "ents_setup", 20000, "40",       "real", "struct array scan and filter"),
    ("real_grid",     "grid_setup", 200,   "200, 20",  "real", "flood fill over a 2D grid"),
    ("real_names",    None,         0,     "200000",   "real", "build and hash name strings"),
]

def script(variant, kernel, setup, sn, args, reps):
    if variant == "lua":
        head = ("local K = (function()\n"
                + open(f"{ROOT}/bench/guests/lua/kernels.lua").read() + "\nend)()\n")
    else:
        head = ("local M = (function(...)\n" + open(f"{OUT}/k-{variant}.lua").read()
                + "\nend)({})\n"
                # TinyGo's wasm-unknown output is a REACTOR: globals and the
                # heap are set up by _initialize, and every export traps on
                # `unreachable` until it has run.
                + "if M.exports['_initialize'] then M.exports['_initialize']() end\n"
                + "local K = setmetatable({}, {__index=function(_,k) return M.exports[k] end})\n")
    body = f"K['{setup}']({sn})\n" if setup else ""
    body += f"local r for _ = 1, {reps} do r = K['{kernel}']({args}) end\n"
    body += "print(string.format('%.10g', r))\n" if reps else "print('setup-only')\n"
    return head + body

def timeit(variant, kernel, setup, sn, args):
    """Best-of-RUNS at REPS iterations, minus best-of-RUNS at zero.

    The subtraction removes process startup, the chunk parse, _initialize and
    the setup call exactly -- all of which are constant and none of which are
    the kernel. Timing comes from outside the process because lua52f models
    Factorio's sandbox, which has no `os` library to read a clock from.
    """
    def once(reps):
        path = f"{OUT}/run-{variant}.lua"
        open(path, "w").write(script(variant, kernel, setup, sn, args, reps))
        best, out = 1e9, None
        for _ in range(RUNS):
            t = time.perf_counter()
            p = subprocess.run([LUA, path], capture_output=True, text=True)
            if p.returncode != 0:
                sys.exit(f"{variant}/{kernel} failed:\n{p.stdout}{p.stderr}")
            best, out = min(best, time.perf_counter() - t), p.stdout.strip()
        return best, out
    hi, sums = once(REPS)
    lo, _ = once(0)
    return (hi - lo) / REPS, sums

VARIANTS = [("lua", "Lua"), ("go", "TinyGo"), ("rs", "Rust")]
print()
print(f"  {'kernel':<15}{'Lua':>9}{'TinyGo':>9}{'Rust':>9}   {'Go/Lua':>8}{'Rust/Lua':>9}")
print("  " + "-" * 64)
failed = False
for cat in ("pure", "real"):
    label = "PURE -- tight loops, no allocation" if cat == "pure" \
            else "REALISTIC -- what a mod inner loop does"
    print(f"  {label}")
    for kernel, setup, sn, args, c, desc in KERNELS:
        if c != cat:
            continue
        t, sums = {}, set()
        for v, _ in VARIANTS:
            t[v], s = timeit(v, kernel, setup, sn, args)
            sums.add(s)
        if len(sums) > 1:
            print(f"  {kernel:<15} *** CHECKSUMS DIFFER: {sorted(sums)} ***")
            failed = True
            continue
        print(f"  {kernel:<15}{t['lua']*1000:>8.1f}m{t['go']*1000:>8.1f}m{t['rs']*1000:>8.1f}m"
              f"   {t['go']/t['lua']:>7.2f}x{t['rs']/t['lua']:>8.2f}x")
    print()
if failed:
    sys.exit("a kernel disagreed across languages; the timings above are not comparable")
print("  checksums agree across all three languages")
PY
