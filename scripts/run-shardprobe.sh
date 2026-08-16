#!/usr/bin/env bash
# Run the FkLua sharded-linear-memory probe inside a real Factorio, headlessly.
#
# EVERY WALL-RELATED NUMBER MUST COME FROM THE GAME. bin/lua52f is stock 5.2.1,
# whose array part grows to 2^30; it cannot see the 4 MiB wall in either
# direction (agents/sandbox.md, "The 2^20 table wall"). This script is the only
# valid instrument for anything in agents/sharding.md.
#
# The probe is a BARE LUA MOD -- no wasm, no emitter, no --persist, no
# collector -- for the same reason the wall itself was established that way:
# so that none of the result is attributable to FkLua.
#
#   FACTORIO_USERDIR=/tmp/fkshard ./scripts/run-shardprobe.sh
#
# Timings come out of factorio-current.log as FKSHARD lines, because
# helpers.create_profiler() is the only clock in the sandbox and it refuses to
# hand Lua a raw number.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

MODDIR="$ROOT/testdata/tmp/shardprobe-mods"
TMPDIR="$ROOT/testdata/tmp"
MAP="$TMPDIR/shardprobe-map.zip"
OUT="$ROOT/testdata/tmp/shardprobe"
TICKS="${TICKS:-4}"
RUNS="${RUNS:-1}"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"

# A path with a newline in it is how stage D created three directories NAMED BY
# SHELL OUTPUT. Refuse rather than quote harder.
check_path() {
  case "$2" in
    "") echo "internal error: $1 is empty" >&2; exit 1 ;;
    *"
"*) echo "internal error: $1 contains a NEWLINE" >&2; exit 1 ;;
  esac
}
check_path ROOT "$ROOT"
check_path FACTORIO_BIN "$FACTORIO"

# Factorio LOCKS its user directory, so this dies at startup if a game is open.
# FACTORIO_USERDIR alone only says where to READ logs; the game needs -c with a
# config.ini whose write-data says the same thing.
CFGARG=()
if [ -n "${FACTORIO_USERDIR:-}" ]; then
  check_path FACTORIO_USERDIR "$USERDIR"
  mkdir -p "$USERDIR/config"
  CFG="$USERDIR/config/config.ini"
  if [ ! -f "$CFG" ] || ! grep -q "^write-data=$USERDIR\$" "$CFG"; then
    DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
    if [ -f "$DEFAULT_CFG" ]; then
      sed -e "s|^write-data=.*|write-data=$USERDIR|" "$DEFAULT_CFG" > "$CFG"
    else
      printf '[path]\nread-data=__PATH__executable__/../data\nwrite-data=%s\n' "$USERDIR" > "$CFG"
    fi
  fi
  CFGARG=(-c "$CFG")
fi

echo "==> assembling mod directory"
rm -rf "$MODDIR" "$MAP"
mkdir -p "$MODDIR" "$TMPDIR" "$OUT"
cp -R "$ROOT/testdata/shardprobe/fklua-shardprobe" "$MODDIR/fklua-shardprobe_0.0.1"
stamp_series "$MODDIR/fklua-shardprobe_0.0.1"
cat > "$MODDIR/mod-list.json" <<'JSON'
{"mods":[{"name":"base","enabled":true},{"name":"fklua-shardprobe","enabled":true}]}
JSON

echo "==> creating throwaway map"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --create "$MAP" \
  --map-gen-seed 1 --disable-audio >"$TMPDIR/shardprobe-create.log" 2>&1 \
  || { echo "map creation failed; see $TMPDIR/shardprobe-create.log" >&2
       tail -30 "$TMPDIR/shardprobe-create.log" >&2; exit 1; }

echo "==> running probe (this takes a minute; the 8 MiB flat build alone is ~5 s)"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --benchmark "$MAP" \
  --benchmark-ticks "$TICKS" --benchmark-runs "$RUNS" --disable-audio \
  >"$TMPDIR/shardprobe-run.log" 2>&1 \
  || { echo "the probe run failed; see $TMPDIR/shardprobe-run.log" >&2
       tail -30 "$TMPDIR/shardprobe-run.log" >&2; exit 1; }

# DID THE MOD LOAD? Two tables of worst ticks measured against the BASE GAME
# with no mod in it were published before stage D added checks like these, and
# neither table looked wrong.
LOG="$USERDIR/factorio-current.log"
[ -f "$LOG" ] || LOG="$TMPDIR/shardprobe-run.log"
grep -q "Checksum for script __fklua-shardprobe__" "$TMPDIR/shardprobe-run.log" \
  || { echo "Factorio never loaded the probe mod -- every number would be the" >&2
       echo "  BASE GAME. See $TMPDIR/shardprobe-run.log" >&2; exit 1; }
grep -q "FKSHARD_END" "$LOG" "$TMPDIR/shardprobe-run.log" 2>/dev/null \
  || { echo "the probe loaded but never finished: no FKSHARD_END. See $LOG" >&2
       grep -c FKSHARD "$LOG" >&2 || true; exit 1; }

grep -h "FKSHARD" "$LOG" "$TMPDIR/shardprobe-run.log" 2>/dev/null | sort -u > "$OUT/timings.txt" || true
cp "$LOG" "$OUT/factorio.log" 2>/dev/null || true

echo "==> $(grep -c FKSHARD "$OUT/timings.txt") FKSHARD lines in $OUT/timings.txt"
python3 "$ROOT/scripts/analyze-shardprobe.py" "$OUT/timings.txt"

# THE ORACLE LEG. The same two loops on the same below-wall table under
# bin/lua52f, which is where every host-side collection number in agents/gc.md
# was derived. The ratio between this and the in-game `ld_ctl`/`ld_flat` rows
# above is what carries a host-side number into the game -- see
# agents/sharding.md §2, which is the answer to stage D's open item 1. lua52f
# has no clock, so the PROCESS is timed and the two modes differ by exactly one
# table read.
if [ -x "$ROOT/bin/lua52f" ]; then
  echo
  echo "==> the same loops under bin/lua52f (best of 5; no wall in this interpreter)"
  for m in control access; do
    best=""
    for _ in 1 2 3 4 5; do
      t="$( { /usr/bin/time -p "$ROOT/bin/lua52f" \
              "$ROOT/testdata/shardprobe/oracle-access.lua" "$m" >/dev/null; } 2>&1 \
            | awk '/^real/{print $2}')"
      if [ -z "$best" ] || awk -v a="$t" -v b="$best" 'BEGIN{exit !(a<b)}'; then best="$t"; fi
    done
    awk -v m="$m" -v t="$best" \
      'BEGIN{printf "    %-8s %6.3f s over 10,000,000 iterations = %5.1f ns each\n", m, t, t/1e7*1e9}'
  done
  echo "    (the difference of the two is the TABLE READ; compare with the"
  echo "     ld_ctl / ld_flat rows above, which are the identical loops in game)"
else
  echo "==> bin/lua52f is not built; the oracle leg is SKIPPED. \`make lua52f\`" >&2
fi
