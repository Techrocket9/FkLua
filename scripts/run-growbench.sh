#!/usr/bin/env bash
# What a `memory.grow` costs the WORST TICK, in a real Factorio, measured by
# Factorio -- PER TICK, and per GROW.
#
# This is the acceptance gate for the grow pacing, and it is run-gcbench.sh's
# instrument pointed at a different quantity. Everything structural here --
# --benchmark-verbose with header-driven column parsing, the load tick dropped
# as a row rather than swallowed by a maximum, four independent "did the mod
# actually load" checks, paths derived from the arm name so no stray stdout can
# become part of a --mod-directory -- is that script's, for the reasons its
# header gives at length. What is new is the correlation:
#
#   THE GUEST LOGS THE TICK OF EVERY GROW, and this script pulls exactly those
#   rows out of the per-tick CSV. A worst-tick number cannot say WHY it was the
#   worst; a grow-tick distribution beside an every-other-tick distribution can.
#   The offset between the guest's game tick and the CSV's t<N> is derived from
#   the guest's own first logged tick rather than assumed.
#
# SIX ARMS: three heap targets x two growth laws, because the two laws scale
# differently and only one of them is FkLua's to change.
#
#   leak4/16/40     -gc=leaking  -- TinyGo's growHeap DOUBLES
#   coll4/16/40     -gc=custom   -- fkgc grows by a capped quarter
#
# FACTORIO LOCKS ITS USER DIRECTORY, so set FACTORIO_USERDIR if a game is open.
#
#   ARMS="coll40 leak40" TICKS=6000 RUNS=1 ./scripts/run-growbench.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"
WORK="$ROOT/testdata/tmp"
# 6,000 ticks is what the 40 MiB arm needs at 8 KiB a tick, with headroom.
TICKS="${TICKS:-6000}"
RUNS="${RUNS:-1}"
OPT="${OPT:-3}"
SKIP="${SKIP:-1}"
COUNTERS="${COUNTERS:-wholeUpdate,scriptUpdate,luaGarbageIncremental}"

check_path() {
  local what="$1" p="$2"
  case "$p" in
    "") echo "internal error: $what is empty" >&2; exit 1 ;;
    *"
"*) echo "internal error: $what contains a NEWLINE" >&2; exit 1 ;;
  esac
}

check_path "ROOT" "$ROOT"
check_path "WORK" "$WORK"
check_path "FACTORIO_BIN" "$FACTORIO"
[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"
mkdir -p "$WORK"

CFGARG=()
if [ -n "${FACTORIO_USERDIR:-}" ]; then
  check_path "FACTORIO_USERDIR" "$USERDIR"
  mkdir -p "$USERDIR/config"
  CFG="$USERDIR/config/config.ini"
  if [ ! -f "$CFG" ] || ! grep -q "^write-data=$USERDIR\$" "$CFG"; then
    DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
    if [ -f "$DEFAULT_CFG" ]; then
      sed -e "s|^write-data=.*|write-data=$USERDIR|" "$DEFAULT_CFG" > "$CFG"
    else
      printf '[path]\nread-data=__PATH__executable__/../data\nwrite-data=%s\n' \
        "$USERDIR" > "$CFG"
    fi
  fi
  CFGARG=(-c "$CFG")
fi

arm_dir()  { printf '%s/growbench-mods-%s' "$WORK" "$1"; }
arm_map()  { printf '%s/growbench-map-%s.zip' "$WORK" "$1"; }
arm_wasm() { printf '%s/growbench-%s.wasm' "$WORK" "$1"; }
arm_mod()  { printf 'fk-growbench-%s' "$1"; }

# THE WASM IS ALWAYS REBUILT. agents/testing.md, "A build cache keyed on
# nothing is not a cache": run-roundtrip.sh's `if [ ! -f "$wasm" ]` cost
# sharding stage C four wrong conclusions in a row, each drawn from the binary
# the FIRST invocation had built. There is no cache here at all -- a warm TinyGo
# build is under a second and a stale guest is a silent lie.
build_arm() {
  local name="$1" tggc="$2" fkgc="$3" tags="$4"
  local wasm dir map mod
  wasm="$(arm_wasm "$name")"; dir="$(arm_dir "$name")"
  map="$(arm_map "$name")";   mod="$(arm_mod "$name")"
  check_path "the $name wasm path" "$wasm"
  check_path "the $name mod directory" "$dir"
  check_path "the $name map path" "$map"

  echo "==> building $name (-gc=$tggc${tags:+ -tags $tags}, --gc=$fkgc)" >&2
  local tagargs=()
  [ -n "$tags" ] && tagargs=(-tags "$tags")
  rm -f "$wasm"
  ( cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
      "-gc=$tggc" -opt=2 "${tagargs[@]}" -o "$wasm" ./examples/growbench )
  [ -f "$wasm" ] || { echo "tinygo produced no $wasm" >&2; return 1; }

  rm -rf "$dir"
  mkdir -p "$dir"
  go run "$ROOT/cmd/fklua" mod "$wasm" --opt="$OPT" --gc="$fkgc" \
    --factorio-version "$SERIES" \
    --name "$mod" --version 0.1.0 --author FkLua \
    --description "FkLua memory.grow worst-tick benchmark ($name)" \
    -o "$dir" >/dev/null
  cat > "$dir/mod-list.json" <<JSON
{"mods":[{"name":"base","enabled":true},{"name":"$mod","enabled":true}]}
JSON
  [ -f "$dir/${mod}_0.1.0/control.lua" ] || {
    echo "packaging $name produced nothing in $dir" >&2; return 1; }

  # The map is created WITH the mod so on_init runs outside the window. Unlike
  # gcbench this guest builds nothing in on_init -- the growth is the whole
  # benchmark -- but the map is still rebuilt every time, because a map created
  # against a different guest is the same stale-input failure as a cached wasm.
  rm -f "$map"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --create "$map" \
    --map-gen-seed 1 --disable-audio >"$WORK/growbench-create-$name.log" 2>&1 || {
      echo "map creation failed for $name; see $WORK/growbench-create-$name.log" >&2
      tail -20 "$WORK/growbench-create-$name.log" >&2; return 1; }
  grep -q "\[growbench\]" "$WORK/growbench-create-$name.log" || {
    echo "the guest never logged during --create for $name: the map was made" >&2
    echo "  WITHOUT the mod." >&2; return 1; }
}

run_arm() {
  local name="$1"
  local dir map mod log
  dir="$(arm_dir "$name")"; map="$(arm_map "$name")"; mod="$(arm_mod "$name")"
  log="$WORK/growbench-run-$name.log"

  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$map" \
    --benchmark-ticks "$TICKS" --benchmark-runs "$RUNS" \
    --benchmark-verbose "$COUNTERS" --disable-audio >"$log" 2>&1 || {
      echo "    the benchmark itself failed; see $log" >&2
      tail -20 "$log" >&2; return 1; }

  grep -q "Checksum for script __${mod}__" "$log" || {
    echo "    Factorio did not load $mod at all -- every number below would be" >&2
    echo "    the BASE GAME. See $log" >&2; return 1; }
  grep -q "\[growbench\]" "$log" || {
    echo "    $mod loaded but the guest never logged. See $log" >&2; return 1; }
  if grep -qE "stack traceback|Error while running" "$log"; then
    echo "    a script error inside the benchmark window; see $log" >&2
    grep -nE "Error" "$log" | head >&2; return 1
  fi
  # THE ANSWER DID NOT MOVE. A pre-build that materialised a slot at the wrong
  # index, or a grow that skipped words it should have zeroed, shows up here and
  # NOWHERE ELSE -- the memory is still addressable and the run simply produces
  # different bytes.
  if grep -q "ok=0" "$log"; then
    echo "    $name CHANGED ITS ANSWER: the guest's checksum moved. See $log" >&2
    return 1; fi

  python3 "$ROOT/scripts/analyze-growbench.py" "$log" "$name" "$SKIP" || return 1
}

echo "==> $TICKS ticks x $RUNS runs, -opt=$OPT, dropping t<$SKIP, counters: $COUNTERS"
FAILED=0
for name in ${ARMS:-coll4 coll16 coll40 leak4 leak16 leak40}; do
  case "$name" in
    leak4)   build_arm leak4   leaking leaking ""                || { FAILED=1; continue; } ;;
    leak16)  build_arm leak16  leaking leaking "gb16"            || { FAILED=1; continue; } ;;
    leak40)  build_arm leak40  leaking leaking "gb40"            || { FAILED=1; continue; } ;;
    coll4)   build_arm coll4   custom collected "gbcollect"      || { FAILED=1; continue; } ;;
    coll16)  build_arm coll16  custom collected "gbcollect,gb16" || { FAILED=1; continue; } ;;
    coll40)  build_arm coll40  custom collected "gbcollect,gb40" || { FAILED=1; continue; } ;;
    *) echo "unknown arm: $name (want leak4|leak16|leak40|coll4|coll16|coll40)" >&2
       FAILED=1; continue ;;
  esac
  echo "==> $name (mods: $(arm_dir "$name"))"
  run_arm "$name" || FAILED=1
done

echo
if [ "$FAILED" -ne 0 ]; then
  echo "ONE OR MORE ARMS FAILED -- do not quote anything above." >&2
  exit 1
fi
cat <<'NOTE'
WHAT THESE NUMBERS ARE.
scriptUpdate is the guest's whole tick. The GROW rows are the ticks the guest
itself reported a memory.grow on; the OTHER rows are every remaining tick of the
same run. The difference between the two distributions is what a grow costs,
measured rather than inferred from a maximum.

The load tick is dropped and reported separately. It is not a grow: it is
control.lua rebuilding the shard vector from the save, which agents/guests.md
prices at ~26 ms/MiB flat.
NOTE
