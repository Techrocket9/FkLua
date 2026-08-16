#!/usr/bin/env bash
# What `memory.grow`'s zero-fill costs, in a real Factorio, per word and per
# CALL -- measured in a bare Lua mod with no guest in it.
#
# WHY THIS IS NOT A HOST-SIDE MEASUREMENT. The quantity is what it costs
# Factorio's Lua to CREATE a table slot. `bin/lua52f` is stock 5.2.1 and reads a
# table 4-6x faster than the game does even below any wall (agents/sharding.md
# section 2), so a host-side ns/word would understate this by exactly the
# unknown this repo spent a milestone measuring. Same rule as the shard probe,
# and the same shape of instrument.
#
# WHAT COMES OUT. Four numbers, and the design hangs on the relation between
# them:
#
#   build_<inc>    40 MiB reached by <inc>-word grows -- the TOTAL cost of an
#                  increment policy, i.e. what a smaller increment costs in
#                  fixed overhead across the whole climb.
#   grow_<inc>@S   one grow of <inc> words at heap S -- the WORST TICK it leaves.
#   splice_<inc>@S the same grow when the words are already materialised -- what
#                  a pre-build that kept up reduces the growing tick to.
#   paced_shard@S  a shard built in 8,192-word pieces against oneshot_shard@S,
#                  the same shard in one go -- the overhead pacing itself costs.
#
#   FACTORIO_USERDIR=/tmp/fkgrow RUNS=2 ./scripts/run-growprobe.sh
#
# Timings come out of factorio-current.log as FKGROW lines, because
# helpers.create_profiler() is the only clock in the sandbox and it refuses to
# hand Lua a raw number.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

MODDIR="$ROOT/testdata/tmp/growprobe-mods"
TMPDIR="$ROOT/testdata/tmp"
MAP="$TMPDIR/growprobe-map.zip"
OUT="$TMPDIR/growprobe"
TICKS="${TICKS:-4}"
RUNS="${RUNS:-1}"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"

# A path with a newline in it is how sharding stage C created three directories
# NAMED BY SHELL OUTPUT. Refuse rather than quote harder.
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
cp -R "$ROOT/testdata/growprobe/fklua-growprobe" "$MODDIR/fklua-growprobe_0.0.1"
stamp_series "$MODDIR/fklua-growprobe_0.0.1"
cat > "$MODDIR/mod-list.json" <<'JSON'
{"mods":[{"name":"base","enabled":true},{"name":"fklua-growprobe","enabled":true}]}
JSON

echo "==> creating throwaway map"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --create "$MAP" \
  --map-gen-seed 1 --disable-audio >"$TMPDIR/growprobe-create.log" 2>&1 \
  || { echo "map creation failed; see $TMPDIR/growprobe-create.log" >&2
       tail -30 "$TMPDIR/growprobe-create.log" >&2; exit 1; }

echo "==> running probe (each 40 MiB build is ~1 s; the whole set is a minute or two)"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --benchmark "$MAP" \
  --benchmark-ticks "$TICKS" --benchmark-runs "$RUNS" --disable-audio \
  >"$TMPDIR/growprobe-run.log" 2>&1 \
  || { echo "the probe run failed; see $TMPDIR/growprobe-run.log" >&2
       tail -30 "$TMPDIR/growprobe-run.log" >&2; exit 1; }

# DID THE MOD LOAD? Two tables of worst ticks measured against the BASE GAME
# with no mod in it were published before stage D added checks like these, and
# neither table looked wrong.
LOG="$USERDIR/factorio-current.log"
[ -f "$LOG" ] || LOG="$TMPDIR/growprobe-run.log"
grep -q "Checksum for script __fklua-growprobe__" "$TMPDIR/growprobe-run.log" \
  || { echo "Factorio never loaded the probe mod -- every number would be the" >&2
       echo "  BASE GAME. See $TMPDIR/growprobe-run.log" >&2; exit 1; }
grep -hq "FKGROW_END" "$LOG" "$TMPDIR/growprobe-run.log" 2>/dev/null \
  || { echo "the probe loaded but never finished: no FKGROW_END. See $LOG" >&2; exit 1; }

grep -h "FKGROW" "$LOG" "$TMPDIR/growprobe-run.log" 2>/dev/null | sort -u > "$OUT/timings.txt" || true
cp "$LOG" "$OUT/factorio.log" 2>/dev/null || true

echo "==> $(grep -c FKGROW "$OUT/timings.txt") FKGROW lines in $OUT/timings.txt"
python3 "$ROOT/scripts/analyze-growprobe.py" "$OUT/timings.txt"
