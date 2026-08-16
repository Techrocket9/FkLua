#!/usr/bin/env bash
# What a collection costs the WORST TICK, in a real Factorio, measured by
# Factorio -- PER TICK.
#
# This is agents/gc.md's stage-C acceptance gate, repaired in stage D. The
# host-side gate DERIVES a paced worst tick, because bin/lua52f is patched to
# Factorio's shape and has no clock: it times a whole collection from outside
# and multiplies by the fraction of that collection's work the collector says
# landed in its worst step. Here there is nothing to derive.
#
# WHAT STAGE C GOT WRONG, and what this script now does instead. `factorio
# --benchmark` prints
#
#     Performed N updates in X ms
#     avg: A ms, min: B ms, max: C ms
#
# per RUN, and stage C read `max` as the worst tick. It is not. Every run begins
# by LOADING the save, which for a guest holding a 2 MiB heap is `_initialize`
# plus unpacking that heap into a Lua word table -- measured at 227 ms against a
# 30 ms collection. `max` was that one tick in both arms and said nothing about
# collecting.
#
# `--benchmark-verbose <counters>` reports one CSV ROW PER TICK instead:
#
#     tick,wholeUpdate,scriptUpdate,...
#     t0,3661000,3450000,...            <- NANOSECONDS
#     t1,412000,201000,...
#
# So the load tick is a row that can be DROPPED rather than a maximum that
# cannot be separated, and what is left is a distribution. The technique --
# --benchmark-verbose with header-driven column parsing and a steady-state
# window -- is ported from the first downstream mod's bench/run.sh, which hit
# this same wall first and solved it; agents/gc.md cites it.
#
# THE HEADER IS READ RATHER THAN COUNTED, because Factorio emits the columns in
# ITS OWN canonical order and not the order the command line asked for. Counting
# columns silently relabels them the moment a counter is added.
#
# scriptUpdate is the column, not wholeUpdate: the collector is Lua running in
# on_tick, and wholeUpdate carries the entity and belt updates as noise that is
# the same in both arms and larger than the signal in neither.
#
# TWO ARMS, and they are the same guest with the same allocator, the same
# emitted code and the same collection trigger. The ONLY difference is whether a
# collection runs to completion inside one tick or is cut into bounded steps, so
# everything that is not collection cancels between them and no third baseline
# is needed:
#
#     stw      -gc=custom, fkgc.Collect()   stage B: one whole mark and sweep,
#                                           inline, in an event handler
#     paced    -gc=custom, stage C          one bounded step per tick
#
# A -gc=leaking arm was tried as a baseline and is NOT here, because it is not
# one: under -gc=leaking this guest keeps every node it ever allocated, so its
# linear memory reaches tens of MiB over the run and Lua's own collector walks
# all of it -- 20 ms of AVERAGE tick against the collected arms' 0.2. It measures
# the thing this feature exists to prevent, not a floor to subtract.
#
# THE MAP IS CREATED WITH THE MOD LOADED, per arm, and that is not a detail. The
# guest builds its 44,000-node live set in fk_on_init, which is a 5.2-SECOND tick
# -- and a benchmark that included it would be reporting that and nothing else.
# Creating the map with the mod present runs on_init at creation time and saves
# the warm heap, so the benchmark starts from a guest already holding its live
# set.
#
# The live set (44,000 nodes, ~2.0 MiB) is chosen to put the heap near the
# 2.39 MiB row of agents/gc.md's stage-B pause table, which that table prices at
# 32.39 ms stopped. This is that row, reproduced in the game.
#
# DID THE MOD LOAD? Asked four ways, and every one of them is fatal. Stage C
# published a full table of worst ticks measured against the base game with no
# guest in it, TWICE, and neither table looked wrong. The cause is now fixed at
# the ROOT rather than only detected: build_arm no longer returns its directory
# on STDOUT. One stray progress line there made `--mod-directory` a two-line
# string, Factorio silently ignored it, and the paths in the transcript still
# looked right because their first line was. Directories are now derived from
# the arm name by the builder and the runner alike, and every path is asserted
# to contain no newline before it is handed to the game -- which is also the
# defect that left three directories NAMED BY SHELL OUTPUT in the tree (8b917b9).
#
# FACTORIO LOCKS ITS USER DIRECTORY, so set FACTORIO_USERDIR if a game is open.
#
#   TICKS=1200 RUNS=3 OPT=3 SKIP=1 FRESH=1 ./scripts/run-gcbench.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"
WORK="$ROOT/testdata/tmp"
TICKS="${TICKS:-1200}"
RUNS="${RUNS:-3}"
OPT="${OPT:-3}"
# How many ticks to drop from the head of EVERY run. 1 drops the LOAD TICK,
# which is the whole reason this script was rewritten; it is reported as its own
# row rather than quietly folded away.
SKIP="${SKIP:-1}"
# The counters asked for. wholeUpdate is kept so a reader can see what fraction
# of the tick the guest is, and luaGarbageIncremental so that Lua's own
# collector is visibly not the thing being measured.
COUNTERS="${COUNTERS:-wholeUpdate,scriptUpdate,luaGarbageIncremental}"

# Every path this script builds is interpolated into a Factorio command line and
# some of them into JSON. A newline in one is how stage C created three
# directories NAMED BY SHELL OUTPUT; they rode into a commit, each holding a
# mod-list.json, and nothing failed. Refuse rather than quote harder: a path
# with a newline in it is a bug upstream of here every time.
check_path() {
  local what="$1" p="$2"
  case "$p" in
    "") echo "internal error: $what is empty" >&2; exit 1 ;;
    *"
"*) echo "internal error: $what contains a NEWLINE, which is how stage C" >&2
        echo "  created directories named by shell output. Value was:" >&2
        printf '  %q\n' "$p" >&2
        exit 1 ;;
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

# Factorio LOCKS its user directory, so an in-game run started while the game is
# open dies at startup. FACTORIO_USERDIR alone is not enough -- it only tells
# this script where to read logs from -- so the config.ini the GAME is handed
# has to say the same write-data. Copied from the installed one rather than
# written from scratch, because read-data is not guessable from the executable's
# path on every platform, which cost a run: a hand-written
# `__PATH__executable__/../../data` deduced the wrong directory and every
# invocation failed with "there is no package core".
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

# Paths are a FUNCTION OF THE ARM NAME, computed identically by the builder and
# the runner. Stage C returned the mod directory from build_arm on stdout, so
# any stray line there became part of a `--mod-directory` argument. There is no
# stdout return channel any more, and no caller-side variable to get it wrong.
arm_dir()  { printf '%s/gcbench-mods-%s' "$WORK" "$1"; }
arm_map()  { printf '%s/gcbench-map-%s.zip' "$WORK" "$1"; }
arm_wasm() { printf '%s/gcbench-%s.wasm' "$WORK" "$1"; }
arm_mod()  { printf 'fk-gcbench-%s' "$1"; }

# Build one arm: tinygo flags, then package with the matching --gc, then create
# a map WITH the mod loaded so fk_on_init is outside the benchmark window.
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
  ( cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
      "-gc=$tggc" -opt=2 "${tagargs[@]}" -o "$wasm" ./examples/gcbench )

  rm -rf "$dir"
  mkdir -p "$dir"
  go run "$ROOT/cmd/fklua" mod "$wasm" --opt="$OPT" --gc="$fkgc" \
    --factorio-version "$SERIES" \
    --name "$mod" --version 0.1.0 --author FkLua \
    --description "FkLua stage-D worst-tick benchmark ($name)" \
    -o "$dir" >/dev/null
  cat > "$dir/mod-list.json" <<JSON
{"mods":[{"name":"base","enabled":true},{"name":"$mod","enabled":true}]}
JSON
  [ -f "$dir/${mod}_0.1.0/control.lua" ] || {
    echo "packaging $name produced nothing in $dir" >&2; return 1; }

  # The map is created WITH this arm's mod, so fk_on_init -- which builds the
  # 44,000-node live set and costs 5.2 seconds -- happens here rather than
  # inside the window whose distribution is the whole point.
  #
  # Kept if it is already there: the map is a pure function of the guest and the
  # seed. Rebuild it with FRESH=1 after changing the guest.
  if [ -f "$map" ] && [ -z "${FRESH:-}" ]; then
    echo "    (reusing $map; FRESH=1 to rebuild)" >&2
    return 0
  fi
  rm -f "$map"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --create "$map" \
    --map-gen-seed 1 --disable-audio >"$WORK/gcbench-create-$name.log" 2>&1 || {
      echo "map creation failed for $name; see $WORK/gcbench-create-$name.log" >&2
      tail -20 "$WORK/gcbench-create-$name.log" >&2; return 1; }
  # on_init is where the live set is built, and it logs. A map created WITHOUT
  # the guest is a map whose saved heap is empty, and every number later
  # measured against it would be the base game -- which is exactly what happened
  # twice, undetected.
  grep -q "\[gcbench\]" "$WORK/gcbench-create-$name.log" || {
    echo "the guest never logged during --create for $name: the map was made" >&2
    echo "  WITHOUT the mod, so its saved heap is empty." >&2; return 1; }
}

# Pull one --benchmark-verbose column out of a log, one value per line, with the
# first SKIP ticks of every run dropped.
#
# HEADER-DRIVEN, not positional: Factorio emits the counters in its own
# canonical order rather than the order asked for, so counting columns silently
# reads the wrong one as soon as a counter is added.
#
# Each --benchmark-runs pass restarts the tick numbering at t0, so `t < skip`
# drops the load tick of EVERY run rather than only the first.
tick_column() {
  local log="$1" want="$2" skip="$3"
  awk -F, -v want="$want" -v skip="$skip" '
    /^tick,/ { for (i = 2; i <= NF; i++) if ($i != "") col[$i] = i; seen = 1; next }
    /^t[0-9]+,/ {
      if (!seen || !(want in col)) next
      t = substr($1, 2) + 0
      if (t < skip) next
      print $col[want] + 0
    }
  ' "$log"
}

# mean / median / p90 / p99 / worst over a stream of NANOSECONDS, printed as ms.
# Sorted by sort(1) rather than in awk, because macOS awk has no asort.
percentiles() {
  sort -n | awk '
    { v[n++] = $1 }
    END {
      if (n == 0) exit 1
      for (i = 0; i < n; i++) s += v[i]
      printf "%d %.4f %.4f %.4f %.4f %.4f\n", n, s / n / 1e6,
        v[int(n * 0.50)] / 1e6, v[int(n * 0.90)] / 1e6,
        v[int(n * 0.99)] / 1e6, v[n - 1] / 1e6
    }'
}

# Run one arm and report the per-tick scriptUpdate distribution.
run_arm() {
  local name="$1"
  local dir map mod log
  dir="$(arm_dir "$name")"; map="$(arm_map "$name")"; mod="$(arm_mod "$name")"
  log="$WORK/gcbench-run-$name.log"
  check_path "the $name mod directory" "$dir"
  check_path "the $name map path" "$map"

  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$map" \
    --benchmark-ticks "$TICKS" --benchmark-runs "$RUNS" \
    --benchmark-verbose "$COUNTERS" --disable-audio >"$log" 2>&1 || {
      echo "    the benchmark itself failed; see $log" >&2
      tail -20 "$log" >&2; return 1; }

  # DID THE MOD LOAD? Independent answers, all fatal. Two tables of worst ticks
  # measured against the base game were published before the first of these
  # existed, and neither looked wrong.
  grep -q "Checksum for script __${mod}__" "$log" || {
    echo "    Factorio did not load $mod at all -- the mod directory was wrong" >&2
    echo "    and every number below would be the BASE GAME. See $log" >&2
    grep -iE "mod-list|Loading mod" "$log" | head -10 >&2
    return 1; }
  grep -q "\[gcbench\]" "$log" || {
    echo "    $mod loaded but the guest never logged: control.lua did not reach" >&2
    echo "    the wasm. See $log" >&2; return 1; }
  if grep -qE "stack traceback|Error while running" "$log"; then
    echo "    a script error inside the benchmark window; see $log" >&2
    grep -nE "Error" "$log" | head >&2; return 1
  fi

  # DID THE ARM COLLECT? The guest's own last word, and it is a GATE rather than
  # a footnote.
  #
  # Stage D found the paced arm sitting in phase 1 for 600 straight ticks with
  # cycles=0 while its heap climbed 2.85 -> 8.68 MB -- marking livelocked, which
  # agents/gc.md calls "the worst failure available to a guest that opted in to
  # one", and which the old avg/min/max instrument reported as a plausible
  # table. An arm that never completed a collection is measuring an allocation
  # loop, so it fails here instead of printing numbers.
  local report cycles phase
  report="$(grep -o "\[gcbench\] tick .*" "$log" | tail -1 || true)"
  [ -n "$report" ] && echo "    $report"
  cycles="$(sed -n 's/.*cycles=\([0-9]*\).*/\1/p' <<<"$report")"
  phase="$(sed -n 's/.*phase=\([0-9]*\).*/\1/p' <<<"$report")"
  if [ -z "$cycles" ] || [ "$cycles" -eq 0 ]; then
    echo "    $name COMPLETED NO COLLECTION over $TICKS ticks (cycles=${cycles:-?}," >&2
    echo "    phase=${phase:-?}). Nothing below is about collecting." >&2
    # TWO DIFFERENT FAILURES WEAR THE SAME cycles=0, and telling a guest author
    # the wrong one sends them to the wrong knob. The guest's own counters
    # separate them, which is why they are in its log line:
    #
    #   phase=2, or stalls=0 with deadlines=0  -- the mark TERMINATED and the
    #     sweep is still running, or the mark is converging and simply has not
    #     got there. That is a run too short for this heap at this budget, and
    #     the arithmetic is in agents/gc.md's reclaim-rate table: ~190 KB/s at
    #     the default 0.5 ms budget, so a 40 MiB heap is ~210 seconds of it.
    #     A longer run or a bigger budget; NOT a smaller allocation rate.
    #
    #   phase=1 with deadlines=0 and a rising stall count -- the mark cannot
    #     converge: the mutator dirties spans faster than the budget re-scans
    #     them. fkgc's forward-progress escape ends it, and if it has not yet,
    #     the run is shorter than the escape's patience.
    local st dl
    st="$(sed -n 's/.*stalls=\([0-9]*\).*/\1/p' <<<"$report")"
    dl="$(sed -n 's/.*deadlines=\([0-9]*\).*/\1/p' <<<"$report")"
    if [ "${phase:-1}" = 2 ] || { [ "${st:-0}" -eq 0 ] && [ "${dl:-0}" -eq 0 ]; }; then
      echo "    This is a RUN TOO SHORT, not a livelock: phase=${phase:-?} with" >&2
      echo "    stalls=${st:-?} and deadlines=${dl:-?} means the mark is converging" >&2
      echo "    (or has already terminated and the sweep is still going). At the" >&2
      echo "    default budget the collector turns over ~190 KB/s, so a heap of" >&2
      echo "    tens of MiB needs tens of thousands of ticks. Raise TICKS, or" >&2
      echo "    raise fkgc.SetBudget in the guest." >&2
    else
      echo "    This is a MARK LIVELOCK: the mutator dirties pages faster than the" >&2
      echo "    budget can re-scan them (stalls=${st:-?}), so no step reaches the" >&2
      echo "    termination attempt. fkgc's forward-progress escape ends it after a" >&2
      echo "    bounded number of windows; deadlines=${dl:-?} says whether it has." >&2
      echo "    The knob is fkgc.SetBudget (bigger) or the guest's allocation rate" >&2
      echo "    (smaller) -- not a longer run. See agents/gc.md, 'Stage D, as built'." >&2
    fi
    return 1
  fi

  local stats
  stats="$(tick_column "$log" scriptUpdate "$SKIP" | percentiles)" || {
    echo "    no per-tick rows in $log -- --benchmark-verbose printed no CSV," >&2
    echo "    so this Factorio does not accept the counter list '$COUNTERS'." >&2
    return 1; }
  local t0
  t0="$(tick_column "$log" scriptUpdate 0 | head -1)"

  # shellcheck disable=SC2086
  set -- $stats
  printf "    %-6s scriptUpdate over %5d ticks   mean %6.3f  median %6.3f  p90 %6.3f  p99 %6.3f  WORST %7.3f ms\n" \
    "$name" "$1" "$2" "$3" "$4" "$5" "$6"
  # The load tick, reported rather than dropped silently. It is the number stage
  # C mistook for a collection.
  awk -v v="${t0:-0}" -v s="$SKIP" \
    'BEGIN{printf "           dropped t<%d; the load tick t0 was %.3f ms of scriptUpdate\n", s, v/1e6}'

  local col line
  for col in wholeUpdate luaGarbageIncremental; do
    line="$(tick_column "$log" "$col" "$SKIP" | percentiles)" || continue
    # shellcheck disable=SC2086
    set -- $line
    printf "           %-22s median %6.3f  WORST %7.3f ms\n" "$col" "$3" "$6"
  done

  # HOW BIG THE GUEST HEAP WAS, which used to be reported as WHICH SIDE OF THE
  # 4 MiB WALL.
  #
  # fk_mod.lua fired a notice when linear memory first passed 4 MiB, and this
  # read it as both discriminator and measurement. THAT NOTICE IS GONE: linear
  # memory is a vector of 2^19-word shards now, no table the guest runs on can
  # reach 2^20 keys at any size, and every sentence the notice printed is false.
  # See agents/sharding.md.
  #
  # So the size comes from the guest's OWN log line instead, which it printed
  # all along -- and it is a better instrument for this script anyway: it is the
  # fkgc heap in bytes rather than a threshold crossing, so both arms report a
  # number rather than one arm reporting silence.
  # THE ANSWER DID NOT MOVE. The huge arms hold ~36 MiB of patterned blocks and
  # re-derive their checksum every 100 ticks; a collector that reclaimed one
  # would show up here and NOWHERE ELSE, because the memory is still addressable
  # and the run would simply produce different bytes. Absent (the non-huge arms
  # carry no bulk) is not a failure; zero is.
  if grep -q "bulkok=0" "$log"; then
    echo "    $name RECLAIMED A LIVE BLOCK: the guest's bulk checksum moved." >&2
    echo "    Nothing else about this run would look wrong. See $log" >&2
    return 1
  fi

  # WHAT THE COLLECTOR CHARGED, which is agents/gc.md's open item 5 made
  # visible: maxstep is the worst PACED step and maxunpaced is the worst burst
  # inside a guest call -- the half MaxStepWork could never see, and the reason
  # the host-side gate read 1.17x of budget while the game read 65x.
  local inst
  inst="$(grep -o "\[gcbench\] tick .*" "$log" | tail -1 || true)"
  local mx mu bd our mt st
  mx="$(sed -n 's/.*maxstep=\([0-9]*\).*/\1/p' <<<"$inst")"
  mu="$(sed -n 's/.*maxunpaced=\([0-9]*\).*/\1/p' <<<"$inst")"
  bd="$(sed -n 's/.*budget=\([0-9]*\).*/\1/p' <<<"$inst")"
  our="$(sed -n 's/.*outruns=\([0-9]*\).*/\1/p' <<<"$inst")"
  mt="$(sed -n 's/.*meta=\([0-9]*\).*/\1/p' <<<"$inst")"
  st="$(sed -n 's/.*deadlines=\([0-9]*\).*/\1/p' <<<"$inst")"
  if [ -n "${bd:-}" ] && [ "${bd:-0}" -gt 0 ]; then
    awk -v m="${mx:-0}" -v u="${mu:-0}" -v b="$bd" -v o="${our:-0}" -v t="${mt:-0}" \
      -v d="${st:-0}" 'BEGIN{printf "           collector: budget %d, worst paced step %d (%.2fx), worst in-call burst %d (%.2fx), %d outruns, %d mark escape(s), %d B metadata\n", b, m, m/b, u, u/b, o, d, t}'
  fi

  local heap
  heap="$(grep -o "heap=[0-9]*" "$log" | tail -1 | cut -d= -f2 || true)"
  if [ -n "$heap" ]; then
    awk -v h="$heap" 'BEGIN{printf "           guest heap: %s B (%.3f MiB)\n", h, h/1048576}'
  else
    printf "           guest heap: the guest logged none\n"
  fi
}

echo "==> $TICKS ticks x $RUNS runs, -opt=$OPT, dropping t<$SKIP, counters: $COUNTERS"
FAILED=0
# ARMS defaults to the stage-D pair. The sharding work adds a second pair --
# the same stop-the-world collection with the guest's linear memory either side
# of the 4 MiB wall -- which is the only instrument that can settle stage D's
# open item 1 (88 ms/MiB in game against 13.9-32.8 host-side, unexplained):
#
#   ARMS="stw stwbig" ./scripts/run-gcbench.sh
#
# fk_mod.lua's wall notice fires in the `big` arms and is silent in the others,
# so a run says out loud which side of the wall each arm measured. `run_arm`
# reports it.
for name in ${ARMS:-stw paced}; do
  case "$name" in
    stw)      build_arm stw      custom collected gcstw       || { FAILED=1; continue; } ;;
    paced)    build_arm paced    custom collected ""          || { FAILED=1; continue; } ;;
    stwbig)   build_arm stwbig   custom collected gcstw,gcbig || { FAILED=1; continue; } ;;
    pacedbig) build_arm pacedbig custom collected gcbig       || { FAILED=1; continue; } ;;
    # The PAST-THE-OLD-CAP arms. ~36 MiB of live bulk, which fkgc.HeapCap made
    # unreachable at any build-tag setting below the 64 MiB one -- and that one
    # cost 583 KiB of .bss before the guest allocated anything. Sharding stage C
    # deleted the cap; these are what say so in the game rather than in a
    # harness. run_arm gates on the guest's own bulkok= line.
    stwhuge)   build_arm stwhuge   custom collected gcstw,gchuge || { FAILED=1; continue; } ;;
    pacedhuge) build_arm pacedhuge custom collected gchuge       || { FAILED=1; continue; } ;;
    *) echo "unknown arm: $name (want stw|paced|stwbig|pacedbig|stwhuge|pacedhuge)" >&2
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
scriptUpdate is the guest's whole tick: the allocation loop, the collection (or
one step of it), and fk_mod.lua's dispatch. Both arms allocate the same amount
and trigger on the same threshold, so the DIFFERENCE between them is the
collection being stop-the-world versus paced, and nothing else.

luaGarbageIncremental is Lua's OWN collector walking the word table. It is not
the guest collector and it does not move between these arms; it is printed so
that nobody attributes it to one.

The load tick is dropped and printed separately. It is `_initialize` plus
unpacking a 2 MiB heap into a Lua word table -- two orders of magnitude over a
collection, and reading it as a worst tick is the defect this script was
rewritten to fix.
NOTE
