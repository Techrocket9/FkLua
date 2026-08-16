#!/usr/bin/env bash
# Run the FkLua day-0 sandbox probe inside a real Factorio, headlessly.
#
# Creates a throwaway map with only the probe mod enabled, runs it for a few
# ticks, and collects script-output/fklua/probe.json plus the FKPROBE_TIME
# lines scraped out of factorio-current.log. Timings have to come from the log
# because helpers.create_profiler() is the only clock in the sandbox and it
# refuses to hand Lua a raw number.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

MODDIR="$ROOT/testdata/moddir"
TMPDIR="$ROOT/testdata/tmp"
MAP="$TMPDIR/probe-map.zip"
OUT="$ROOT/testdata/probe/results"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO" >&2; echo "set FACTORIO_BIN" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"

# Factorio LOCKS its user directory, so this dies at startup if a game is open --
# and reads as a broken probe rather than a busy machine. FACTORIO_USERDIR alone
# only tells the script where to READ logs; the game needs -c with a config.ini
# whose write-data says the same thing. run-roundtrip.sh has done this since M6
# and CLAUDE.md claimed both scripts did; this one did not.
CFGARG=()
if [ -n "${FACTORIO_USERDIR:-}" ]; then
  mkdir -p "$USERDIR/config"
  CFG="$USERDIR/config/config.ini"
  if [ ! -f "$CFG" ]; then
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
cp -R "$ROOT/testdata/probe/fklua-probe" "$MODDIR/fklua-probe_0.0.1"
stamp_series "$MODDIR/fklua-probe_0.0.1"
cat > "$MODDIR/mod-list.json" <<'JSON'
{
  "mods": [
    { "name": "base", "enabled": true },
    { "name": "fklua-probe", "enabled": true }
  ]
}
JSON

echo "==> creating throwaway map"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --create "$MAP" --disable-audio >"$TMPDIR/create.log" 2>&1 \
  || { echo "map creation failed; see $TMPDIR/create.log" >&2; tail -30 "$TMPDIR/create.log" >&2; exit 1; }

echo "==> running probe"
# --benchmark loads the save, runs N ticks and exits. The probe fires on the
# first on_tick, so a handful of ticks is plenty; the extra ticks just let any
# deferred logging flush.
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" \
            --benchmark "$MAP" \
            --benchmark-ticks 20 \
            --benchmark-runs 1 \
            --disable-audio >"$TMPDIR/run.log" 2>&1 \
  || { echo "probe run failed; see $TMPDIR/run.log" >&2; tail -40 "$TMPDIR/run.log" >&2; exit 1; }

echo "==> collecting results"
FOUND=""
for candidate in "$USERDIR/script-output/fklua/probe.json" \
                 "$MODDIR/../script-output/fklua/probe.json" \
                 "$ROOT/script-output/fklua/probe.json"; do
  if [ -f "$candidate" ]; then FOUND="$candidate"; break; fi
done

if [ -z "$FOUND" ]; then
  echo "probe.json not found. Searched:" >&2
  echo "  $USERDIR/script-output/fklua/" >&2
  echo "Run log tail:" >&2
  tail -40 "$TMPDIR/run.log" >&2
  exit 1
fi

cp "$FOUND" "$OUT/probe.json"
echo "    $OUT/probe.json"

# Timing lines and any section failures live in the game log, not in the JSON.
LOG="$USERDIR/factorio-current.log"
if [ -f "$LOG" ]; then
  grep -E "FKPROBE_(TIME|SECTION_FAILED|FATAL|DONE)" "$LOG" > "$OUT/timings.txt" || true
  echo "    $OUT/timings.txt ($(wc -l < "$OUT/timings.txt" | tr -d ' ') lines)"
fi

echo "==> done"
