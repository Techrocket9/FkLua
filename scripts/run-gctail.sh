#!/usr/bin/env bash
# What Lua's OWN collector costs a tick over a vector of shards, over a LONG
# run -- measured in a real Factorio, in a bare Lua mod with no guest in it.
#
# WHY THIS EXISTS. agents/guests.md's cost table publishes "worst tick flat at
# ~0.5 ms, whatever the size", and then two rows that disagree with it and with
# each other: at a 52 MiB sharded heap a 24,000-tick run found 4.178 ms of
# luaGarbageIncremental where a 54.5 MiB run of the same guest found 1.141 ms.
# The file records that honestly -- "not a hard bound" -- with a hypothesis
# (Lua's atomic step) and no measurement behind it. This is the measurement.
#
# WHY NOT run-gcbench.sh. That script drives a real collected GUEST, where the
# heap size, the number of shard tables and the guest's own allocation rate all
# move together and none of them can be held still. Deciding whether the tail
# tracks BYTES, TABLE COUNT or Lua's own cycle needs those varied one at a time,
# which is three numbers in a config file here and is not expressible there.
#
# Same instrument rule as run-growprobe.sh and run-shardprobe.sh: the quantity
# is a property of Factorio's Lua and its table internals, `bin/lua52f` is stock
# 5.2.1 and prices those wrong in both directions, and a mod with no wasm, no
# emitter, no --persist and no collector in it is what makes the answer
# attributable to the REPRESENTATION rather than to FkLua.
#
# THE ARMS, and what each one holds still:
#
#   s16  16 x 2^19  =  32 MiB   heap series: bytes and table count move together,
#   s26  26 x 2^19  =  52 MiB   which is how a real guest grows. s26 is the row
#   s32  32 x 2^19  =  64 MiB   agents/guests.md cannot explain.
#
#   t52  52 x 2^18  =  52 MiB   SAME BYTES, 2x the tables, half the size each
#   q52 104 x 2^17  =  52 MiB   SAME BYTES, 4x the tables
#        -> if the tail is a per-TABLE propagatemark it must FALL across these;
#           if it is the atomic step, which walks everything at once, it must not.
#
#   w26a 26 x 2^19, written every tick across ALL 26 shards
#   w26h  ...the same, across 6 of them
#   w26n  ...the same, across 1 of them
#        -> a store into a BLACK table fires Lua's back-barrier and puts it on
#           grayagain; atomic() ends with propagateall over that list, in ONE
#           indivisible step. So the atomic tick should be the WIDTH of the
#           write set and not the size of the heap, and these three decide it.
#
#   p26  26 x 2^19 + collectgarbage("step", N) every tick
#   w26p w26a + the same pacing, which is the only arm where the lever has a
#        RECURRING event to move rather than a single startup one
#        -> the mitigation lever. agents/guests.md says collectgarbage "moves
#           the pause by less than its own noise, because there is nothing to
#           pace" -- a claim made when the memory was ONE table. Retaken.
#
# A LONG RUN IS THE POINT. The two disagreeing rows are 24,000 ticks each and
# the shorter 1,200-tick runs never saw either, so TICKS defaults to 24000 here.
# A rare tail is not found by a short run and is not disproved by one.
#
#   FACTORIO_USERDIR=/tmp/fktail TICKS=24000 ./scripts/run-gctail.sh
#   ARMS="s26 p26" ./scripts/run-gctail.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"
WORK="$ROOT/testdata/tmp"
OUT="$WORK/gctail"
TICKS="${TICKS:-24000}"
RUNS="${RUNS:-1}"
SKIP="${SKIP:-1}"
COUNTERS="${COUNTERS:-wholeUpdate,luaGarbageIncremental}"

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
[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"
mkdir -p "$WORK" "$OUT"

# Factorio LOCKS its user directory, so a run started while the game is open
# dies at startup and reads as a broken gate. FACTORIO_USERDIR alone only says
# where to READ logs; the game needs -c with a config.ini saying the same
# write-data.
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

# Paths are a FUNCTION OF THE ARM NAME, computed identically everywhere, and
# nothing is returned on stdout. See run-gcbench.sh for the run this cost.
arm_dir() { printf '%s/gctail-mods-%s' "$WORK" "$1"; }
arm_map() { printf '%s/gctail-map-%s.zip' "$WORK" "$1"; }

# shards shardw churn churnw pace writes wshards, per arm. CHURN IS CONSTANT
# ACROSS EVERY ARM on purpose: it is the allocation debt that makes Lua's
# collector step at all, and if it moved between arms the arms would differ in
# two things.
#
# THE w* ARMS ARE THE ONES THAT MATTER and they were added second. Every arm
# above them holds a live set nothing ever WRITES, so each of its tables is
# marked black once and stays black -- and the tail therefore happens exactly
# once per run, at the first atomic step, and never again. A real guest stores
# into its linear memory constantly. `writes` is words stored per tick and
# `wshards` is how many distinct SHARDS they reach, which is the quantity Lua's
# back-barrier actually charges for.
arm_cfg() {
  case "$1" in
    s16) echo "16 524288 16 64 0 0 0" ;;
    s26) echo "26 524288 16 64 0 0 0" ;;
    s32) echo "32 524288 16 64 0 0 0" ;;
    t52) echo "52 262144 16 64 0 0 0" ;;
    q52) echo "104 131072 16 64 0 0 0" ;;
    p26) echo "26 524288 16 64 2 0 0" ;;
    # 52 MiB, written every tick, spread over 1 / 6 / all 26 shards. Same bytes,
    # same allocation debt, same everything -- only the WIDTH of the write set
    # differs, which is the hypothesis.
    w26a) echo "26 524288 16 64 0 256 26" ;;
    w26h) echo "26 524288 16 64 0 256 6" ;;
    w26n) echo "26 524288 16 64 0 256 1" ;;
    # ...and the widest one with collectgarbage pacing on, which is the only
    # arm where the lever has a recurring event to move.
    w26p) echo "26 524288 16 64 2 256 26" ;;
    *)   return 1 ;;
  esac
}

build_arm() {
  local name="$1" dir map cfg
  dir="$(arm_dir "$name")"; map="$(arm_map "$name")"
  check_path "the $name mod directory" "$dir"
  check_path "the $name map path" "$map"
  cfg="$(arm_cfg "$name")" || { echo "unknown arm: $name" >&2; return 1; }
  # shellcheck disable=SC2086
  set -- $cfg

  echo "==> assembling $name ($1 x $2 words, churn $3x$4, pace $5, writes $6 over $7 shards)" >&2
  rm -rf "$dir"
  mkdir -p "$dir"
  cp -R "$ROOT/testdata/gctail/fklua-gctail" "$dir/fklua-gctail_0.0.1"
  stamp_series "$dir/fklua-gctail_0.0.1"
  cat > "$dir/fklua-gctail_0.0.1/config.lua" <<LUA
-- GENERATED by scripts/run-gctail.sh for arm "$name". Do not edit.
return { shards = $1, shardw = $2, churn = $3, churnw = $4, pace = $5,
         writes = $6, wshards = $7 }
LUA
  cat > "$dir/mod-list.json" <<'JSON'
{"mods":[{"name":"base","enabled":true},{"name":"fklua-gctail","enabled":true}]}
JSON

  # The map is a pure function of the seed here -- the live set is built at
  # CHUNK LOAD rather than in on_init, so nothing about it is saved and one map
  # would do for every arm. It is still created per arm so that a failure to
  # load the mod is caught at creation, where the log is short.
  rm -f "$map"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --create "$map" \
    --map-gen-seed 1 --disable-audio >"$WORK/gctail-create-$name.log" 2>&1 || {
      echo "map creation failed for $name; see $WORK/gctail-create-$name.log" >&2
      tail -20 "$WORK/gctail-create-$name.log" >&2; return 1; }
  grep -q "FKGCTAIL_SETUP" "$WORK/gctail-create-$name.log" || {
    echo "the probe never logged during --create for $name: the map was made" >&2
    echo "  WITHOUT the mod, so every number below would be the BASE GAME." >&2
    return 1; }
}

run_arm() {
  local name="$1" dir map log
  dir="$(arm_dir "$name")"; map="$(arm_map "$name")"
  log="$WORK/gctail-run-$name.log"

  echo "==> running $name ($TICKS ticks x $RUNS)" >&2
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$map" \
    --benchmark-ticks "$TICKS" --benchmark-runs "$RUNS" \
    --benchmark-verbose "$COUNTERS" --disable-audio >"$log" 2>&1 || {
      echo "    the benchmark failed; see $log" >&2; tail -20 "$log" >&2; return 1; }

  # DID THE MOD LOAD? Two tables of worst ticks measured against the BASE GAME
  # were published before run-gcbench.sh grew checks like these, and neither
  # table looked wrong. Every one of these is fatal.
  grep -q "Checksum for script __fklua-gctail__" "$log" || {
    echo "    Factorio did not load the probe -- every number would be the" >&2
    echo "    BASE GAME. See $log" >&2; return 1; }
  grep -q "FKGCTAIL_SETUP" "$log" || {
    echo "    the mod loaded but never built its live set. See $log" >&2; return 1; }
  if grep -qE "stack traceback|Error while running" "$log"; then
    echo "    a script error inside the window; see $log" >&2
    grep -nE "Error" "$log" | head >&2; return 1
  fi
  grep -o "FKGCTAIL_SETUP.*" "$log" | head -1 | sed 's/^/    /'
  cp "$log" "$OUT/$name.log"
}

FAILED=0
ARMS="${ARMS:-s16 s26 s32 t52 q52 p26 w26a w26h w26n w26p}"
for name in $ARMS; do
  build_arm "$name" || { FAILED=1; continue; }
  run_arm "$name" || { FAILED=1; continue; }
done

echo
python3 "$ROOT/scripts/analyze-gctail.py" --skip "$SKIP" \
  $(for n in $ARMS; do [ -f "$OUT/$n.log" ] && printf '%s=%s ' "$n" "$OUT/$n.log"; done)

if [ "$FAILED" -ne 0 ]; then
  echo >&2
  echo "ONE OR MORE ARMS FAILED -- do not quote anything above." >&2
  exit 1
fi
