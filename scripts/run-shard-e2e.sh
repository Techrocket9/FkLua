#!/usr/bin/env bash
# End to end: one packaged guest, three representations, in a real Factorio.
#
# scripts/run-shardprobe.sh measures ACCESS SHAPES in a bare Lua mod. This
# measures the same three shapes through the real emitter, the real
# fk_rt.lua and the real fk_mod.lua, on a guest whose linear memory is a
# parameter -- so the below-wall regression and the above-wall win are both
# stated about a mod rather than about a loop.
#
#   fast   what the emitter PRINTS: the shard-0 fast path, whose test IS the
#          bounds check
#   slow   the control -- every access takes the runtime shard select in
#          ADDITION to the bounds check, synthesised by scripts/shard-e2e-edit.py
#   flat   the pre-sharding form, which the emitter can no longer print, so it
#          needs a pre-sharding compiler binary in FLAT_FKLUA and is SKIPPED
#          without one.
#
#          THE COMMIT IS PINNED HERE SO THE ARM STAYS REPRODUCIBLE. Sharding
#          stage B triaged this as "needs a pre-sharding binary" and left it at
#          that, which is how a control arm quietly becomes a paragraph. The
#          last tree the flat form comes out of is e200fdb, the parent of
#          6422ac0 ("linear memory is a vector of 2^19-word shards"):
#
#              git worktree add /tmp/fklua-flat e200fdb
#              (cd /tmp/fklua-flat && go build -o /tmp/fklua-flat/bin/fklua ./cmd/fklua)
#              FLAT_FKLUA=/tmp/fklua-flat/bin/fklua ./scripts/run-shard-e2e.sh
#
#          The arm is KEPT rather than retired because it is the only thing that
#          measures the merge itself: `slow` shows what an un-merged shard test
#          costs (1.43x) and `flat` shows what the representation costs against
#          not having one (1.007x at 2 MiB, 17.8x at 6). The record of both is
#          in agents/sharding.md section 13 either way.
#
# STAGE A MEASURED THE HAND-EDITED FORM; THIS MEASURES THE EMITTER. The `fast`
# arm is now the shipped one and needs no edit at all, which is the whole point
# of running this again at stage B -- stage A's 17.3x was a prediction about
# code nobody had compiled.
#
# scripts/shard-e2e-edit.py still asserts every replacement matched exactly
# once, so an emitter change breaks the CONTROL loudly rather than quietly
# measuring two copies of the same file.
#
#   FACTORIO_USERDIR=/tmp/fkshard PAGES=96 ./scripts/run-shard-e2e.sh
#   FLAT_FKLUA=/path/to/pre-sharding/fklua PAGES=96 ./scripts/run-shard-e2e.sh
#
# PAGES=96 is 6 MiB (above what USED to be the wall); PAGES=32 is 2 MiB.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"
WORK="$ROOT/testdata/tmp"
PAGES="${PAGES:-96}"
TICKS="${TICKS:-120}"
RUNS="${RUNS:-1}"
OPT="${OPT:-3}"
BYTES=$((PAGES * 65536))

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"

check_path() {
  case "$2" in
    "") echo "internal error: $1 is empty" >&2; exit 1 ;;
    *"
"*) echo "internal error: $1 contains a NEWLINE" >&2; exit 1 ;;
  esac
}
check_path ROOT "$ROOT"
check_path FACTORIO_BIN "$FACTORIO"

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

mkdir -p "$WORK"
WAT="$WORK/e2e-$PAGES.wat"
sed -e "s/@PAGES@/$PAGES/g" -e "s/@BYTES@/$BYTES/g" \
  "$ROOT/testdata/shardprobe/e2e.wat.tmpl" > "$WAT"

# --persist=none on purpose. The three arms differ in how MEM is SHAPED, and
# table mode's `storage.fk_mem IS MEM` aliasing is a separate design question
# (agents/sharding.md, "both persist modes") that this measurement must not
# confound with the cost of an access. `none` rebuilds from data segments every
# load, which is deterministic and stateless -- and the guest's answer does not
# depend on carried state, which is what makes that legitimate here.
build() {
  local arm="$1" dir mod
  dir="$WORK/e2e-mods-$PAGES-$arm"; mod="fk-e2e-$PAGES-$arm"
  check_path "the $arm mod directory" "$dir"
  rm -rf "$dir"; mkdir -p "$dir"
  # The `flat` arm is compiled by a DIFFERENT binary: the pre-sharding emitter.
  # There is no flag that turns sharding off -- agents/sharding.md refused the
  # compile-time gate for good measured reasons -- so the only honest flat arm
  # is a compiler that predates it.
  local FK=(go run "$ROOT/cmd/fklua")
  if [ "$arm" = flat ]; then FK=("$FLAT_FKLUA"); fi
  # --factorio-version in BOTH arms, including the pre-sharding binary: the flag
  # is older than that commit, and a flat arm declaring a different engine
  # series from the sharded one would not be a control.
  "${FK[@]}" mod "$WAT" --opt="$OPT" --persist=none \
    --factorio-version "$SERIES" \
    --name "$mod" --version 0.1.0 --author FkLua \
    --description "FkLua sharding stage-A end-to-end ($arm, $PAGES pages)" \
    -o "$dir" >/dev/null
  local chunk="$dir/${mod}_0.1.0/fk_module.lua"
  [ -f "$chunk" ] || { echo "packaging $arm produced nothing" >&2; return 1; }
  if [ "$arm" = slow ]; then
    python3 "$ROOT/scripts/shard-e2e-edit.py" "$chunk" "$arm"
  fi
  # THE CHECKSUM CHANNEL. The guest cannot log -- it imports nothing -- and the
  # generated control.lua calls hooks and nothing else, so fk_sum is appended
  # here, identically in all three arms. `E` is the export table control.lua
  # already binds.
  cat >> "$dir/${mod}_0.1.0/control.lua" <<'LUA'

script.on_nth_tick(100, function()
  if E.fk_sum then log("fk-e2e-sum=" .. string.format("%.0f", E.fk_sum())) end
end)
LUA
  cat > "$dir/mod-list.json" <<JSON
{"mods":[{"name":"base","enabled":true},{"name":"$mod","enabled":true}]}
JSON
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --create "$WORK/e2e-$PAGES-$arm.zip" \
    --map-gen-seed 1 --disable-audio >"$WORK/e2e-create-$PAGES-$arm.log" 2>&1 || {
      echo "map creation failed for $arm; see $WORK/e2e-create-$PAGES-$arm.log" >&2
      tail -20 "$WORK/e2e-create-$PAGES-$arm.log" >&2; return 1; }
}

tick_column() {
  awk -F, -v want="$2" -v skip="$3" '
    /^tick,/ { for (i = 2; i <= NF; i++) if ($i != "") col[$i] = i; seen = 1; next }
    /^t[0-9]+,/ { if (!seen || !(want in col)) next
                  if (substr($1,2)+0 < skip) next
                  print $col[want] + 0 }' "$1"
}

percentiles() {
  sort -n | awk '{ v[n++] = $1 }
    END { if (n == 0) exit 1
      for (i = 0; i < n; i++) s += v[i]
      printf "%d %.4f %.4f %.4f\n", n, s/n/1e6, v[int(n*0.50)]/1e6, v[n-1]/1e6 }'
}

run() {
  local arm="$1" dir mod log
  dir="$WORK/e2e-mods-$PAGES-$arm"; mod="fk-e2e-$PAGES-$arm"
  log="$WORK/e2e-run-$PAGES-$arm.log"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$WORK/e2e-$PAGES-$arm.zip" \
    --benchmark-ticks "$TICKS" --benchmark-runs "$RUNS" \
    --benchmark-verbose wholeUpdate,scriptUpdate,luaGarbageIncremental \
    --disable-audio >"$log" 2>&1 || {
      echo "    the benchmark failed for $arm; see $log" >&2; tail -20 "$log" >&2; return 1; }
  # DID THE MOD LOAD. Two tables of worst ticks measured against the base game
  # were published before stage D added a check like this, and neither looked
  # wrong.
  grep -q "Checksum for script __${mod}__" "$log" || {
    echo "    Factorio never loaded $mod -- every number would be the BASE GAME" >&2
    return 1; }
  if grep -qE "stack traceback|Error while running" "$log"; then
    echo "    a script error inside the window for $arm; see $log" >&2
    grep -nE "Error" "$log" | head -3 >&2; return 1
  fi
  local stats t0
  stats="$(tick_column "$log" scriptUpdate 1 | percentiles)"
  t0="$(tick_column "$log" scriptUpdate 0 | head -1)"
  # shellcheck disable=SC2086
  set -- $stats
  printf "    %-5s  ticks %4d   mean %9.3f  median %9.3f  worst %9.3f ms   load tick %9.3f ms\n" \
    "$arm" "$1" "$2" "$3" "$4" "$(awk -v v="${t0:-0}" 'BEGIN{print v/1e6}')"
}

ARMS="fast slow"
if [ -n "${FLAT_FKLUA:-}" ]; then
  [ -x "$FLAT_FKLUA" ] || { echo "FLAT_FKLUA is not executable: $FLAT_FKLUA" >&2; exit 1; }
  ARMS="flat $ARMS"
else
  echo "==> no FLAT_FKLUA: skipping the pre-sharding baseline arm"
fi

echo "==> e2e: $PAGES pages = $((BYTES / 1048576)) MiB, -opt=$OPT, $TICKS ticks x $RUNS runs"
for arm in $ARMS; do
  echo "==> building $arm"
  build "$arm"
done
for arm in $ARMS; do
  run "$arm" || exit 1
done

# THE CHECKSUM. A variant that computes a different answer is not a faster
# variant -- CLAUDE.md's rule, and the reason `fklua bench` fails a run on a
# mismatch rather than reporting a flattering number. fk_sum is a pure function
# of the ticks run, so all three arms must agree exactly.
echo "==> checksums (fk_sum after the run; every arm must agree)"
for arm in $ARMS; do
  printf "    %-5s %s\n" "$arm" \
    "$(grep -o "fk-e2e-sum=[0-9]*" "$WORK/e2e-run-$PAGES-$arm.log" | tail -1 || echo "(not logged)")"
done
