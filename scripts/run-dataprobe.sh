#!/usr/bin/env bash
# Ask a real Factorio what it ENFORCES at the settings and data stages, one
# case at a time, and compare every answer with a committed expectation.
#
# WHY THIS EXISTS. `scripts/run-datastage.sh` proves that a guest's data stage
# produces the dump it produced last time. It says nothing about what the engine
# would have REFUSED, because every case it runs is a case that loads. A library
# that generates prototypes has to know the other side: a name the engine will
# not take, an amount past a field's width, a localised string with one
# parameter too many. Those rules are not in runtime-api.json, they are not in
# the wiki in a form anything can check, and every one of them is a load failure
# in a player's hands if a guest emits it. So they are measured here, and the
# measurement is re-runnable rather than remembered.
#
# THE INSTRUMENT IS `--dump-data`, the same one run-datastage.sh uses: it runs
# the settings and data stages and stops, so a case costs about a second and
# never reaches control.lua. The probe mod is bare Lua under
# testdata/dataprobe -- no wasm, no FkLua runtime, no API pin -- because what is
# under test is the ENGINE and a compiler in the path would be a variable.
#
# FOUR OUTCOMES, and the fourth is the reason this is not three:
#
#   loaded    exit 0 and a dump. The case asserts its fact in Lua, so a broken
#             invariant turns the case into a refusal.
#   logged    loaded, and a line the engine wrote is in the output. Some rules
#             are stated in a log line rather than by refusing.
#   refused   non-zero exit, no dump, a first `Error` line. The ordinary shape.
#   aborted   non-zero exit AND the crash handler ran. A fluid amount the
#             fixed-point conversion cannot hold does not refuse; it takes the
#             process down through SIGABRT and hands a player a crash dialog.
#             A probe that folded that into "refused" would be reporting the
#             worst failure the data stage has as the ordinary one.
#
# AN `Error` LINE IS NOT BY ITSELF A REFUSAL. Measured: a stored setting value
# of the wrong type logs `Error StringSetting.cpp:71: ...` and the run still
# exits 0 and writes a dump. So the exit code and the dump are read first and
# the message second, in that order, everywhere below.
#
# THE RUN DECIDES THE OUTCOME, NOT THE EXPECTATION. A run that loaded is
# `logged` whenever the log carries an `Error` line and `loaded` when it does
# not, and the classifier is never told which one the case wanted. Deciding it
# the other way round -- calling every load that was not expected to log
# `loaded` -- would mean an engine that started writing an `Error` line in a
# case this file calls `loaded` produced no signal at all. It now fails, with
# the line printed.
#
# THE EXPECTED TEXT IS A SUBSTRING of the line the engine wrote, because the
# line carries a C++ source location and a property-tree path that are facts
# about the build rather than about the rule. Every line the probe saw is
# printed in full under the table, so a drift in the unpinned part is still in
# front of the reader. For a refused or aborted case the line compared is the
# FIRST `Error` line, which has a consequence worth stating: such a case must
# not also carry a stored value the engine logs about, because the stored-value
# line would come first and the refusal's own line would never be reached. A
# `logged` case is matched against the whole log instead.
#
# A CASE THAT CANNOT BE RUN FAILS. A missing binary, a missing jq, a
# mod-settings writer that is not there yet: each prints a NOT RUN naming the
# remedy and the script exits non-zero. A skipped gate reads exactly like a
# pass, and this one has 41 chances to do it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

PROBE="$ROOT/testdata/dataprobe"
CASES="$PROBE/cases"
EXPECTED="$PROBE/expected.txt"
TMPDIR="$ROOT/testdata/tmp"
MODDIR="$TMPDIR/dataprobe-mods"
TEMPLATE="$TMPDIR/dataprobe-mod"
LOGS="$TMPDIR/dataprobe-logs"
# THE NAME AND VERSION COME OUT OF info.json, because Factorio requires the mod
# DIRECTORY to be `<name>_<version>` and a second spelling of either here is two
# places that have to agree about one fact.
MODNAME="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$PROBE/info.json" | head -1)"
MODVER="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$PROBE/info.json" | head -1)"
# Every engine invocation is bounded. One case aborts on purpose and another
# could hang on a machine this was never run on; a probe that waits forever is
# a probe nobody runs twice.
CASE_TIMEOUT="${FKLUA_DATAPROBE_TIMEOUT:-180}"

if [ ! -x "$FACTORIO" ]; then
  cat >&2 <<EOF

NOT RUN: this probe is Factorio, and there is no Factorio at

  $FACTORIO

Point FACTORIO_BIN at the binary (on macOS/Steam it is inside
factorio.app/Contents/MacOS/) and run this again. Nothing below can be
approximated: the whole point is what the engine does.
EOF
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  cat >&2 <<'EOF'

NOT RUN: jq is not installed, and this probe reads the dump with it.

  brew install jq

A dump is 13 MB of JSON and "the file exists" is not the same claim as "the
engine wrote a data-raw dump", which is what a loaded case is asserting.
EOF
  exit 1
fi

# THE TIMEOUT COMMAND, whose name is not the same everywhere. GNU coreutils
# installs it as `gtimeout` on macOS unless the user asked for unprefixed
# names, and a probe with an unbounded engine run in it is one hung case away
# from being useless.
if command -v timeout >/dev/null 2>&1; then
  TIMEOUT=(timeout "$CASE_TIMEOUT")
elif command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT=(gtimeout "$CASE_TIMEOUT")
else
  cat >&2 <<'EOF'

NOT RUN: neither `timeout` nor `gtimeout` is on PATH.

  brew install coreutils

One case takes the engine down through its crash handler on purpose, and every
run here is bounded so that a case which hangs on some other machine reports a
timeout instead of stopping the probe forever.
EOF
  exit 1
fi

# RE-ASK THE BINARY ITS VERSION, every run, before anything else is measured.
# The installed engine has moved seven times in four weeks on this machine,
# three of them inside one day, so even a same-session memory of it is stale.
ENGINE="$(factorio_version_triple)"
VERSION_LINE="$("$FACTORIO" --version 2>/dev/null | head -1)"
SERIES="$(factorio_series)"

echo "=== what the data stage enforces, asked of a real Factorio ==="
echo "engine:  $VERSION_LINE"
echo "         (info.json will declare $SERIES)"
echo "cases:   $EXPECTED"
echo

# A PRIVATE WRITE-DATA DIRECTORY, so this can run while a Factorio is already
# open: the game LOCKS its user directory and a second process pointed at the
# same one dies at startup, which reads as a broken probe rather than as two
# copies of the game. It is also where --dump-data writes.
CFGARG=()
if [ -n "${FACTORIO_USERDIR:-}" ]; then
  mkdir -p "$USERDIR/config"
  CFG="$USERDIR/config/config.ini"
  if [ ! -f "$CFG" ]; then
    DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
    if [ -f "$DEFAULT_CFG" ]; then
      sed "s|^write-data=.*|write-data=$USERDIR|" "$DEFAULT_CFG" > "$CFG"
    else
      cat > "$CFG" <<EOF
[path]
read-data=__PATH__system-read-data__
write-data=$USERDIR

[general]
locale=auto
EOF
    fi
  fi
  CFGARG=(-c "$CFG")
fi

DUMP="$USERDIR/script-output/data-raw-dump.json"

# write_settings_dat JSON DAT -- encode a case's stored settings into the
# binary file the engine reads before the settings stage.
#
# ONE FUNCTION, so the tool behind it can change without touching a case. It is
# `fklua modsettings write` because a mod-settings.dat is a thing this repo's
# own binary should be able to produce: the format is four version numbers and a
# property tree, the same tier-2 shape fk_abi.lua already speaks, and a gate
# that has to flip a startup setting headlessly has no other way in.
#
# THIS IS THE ONE LINE TO CHANGE if the encoder ever moves. The six cases whose
# expectations were taken with a reference encoder of the same format are
# stored-text-not-trimmed, stored-wrong-type-double, stored-wrong-type-bool,
# stored-number-above-max, stored-number-below-min and stored-dropdown-unoffered;
# every one of them was measured on 2.0.77 and all six passed. While the
# subcommand is absent they report NOT RUN and the script exits 1, which is the
# treatment a missing tool gets everywhere in this repo.
write_settings_dat() {
  "$ROOT/bin/fklua" modsettings write --from "$1" --out "$2"
}

# THE LOG DIRECTORY IS EMPTIED FIRST. The mod-set assertion at the end reads
# `$LOGS/*.log` rather than any one run's output, so a log left behind by a case
# that has since been renamed or deleted would be read as evidence about THIS
# run -- including, in the direction that matters, a `Loading mod space-age` line
# from a run whose mod-list.json was different.
rm -rf "$LOGS"
mkdir -p "$TMPDIR" "$LOGS"

# BUILD THE BINARY, because write_settings_dat is in it. run-datastage.sh
# builds for a sharper reason (a stale binary packages a stale shim); here it is
# the ordinary one, that a subcommand added in the working tree is not in
# whatever bin/fklua happens to be lying around.
echo "building bin/fklua..."
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"

# CAN THE STORED-VALUE CASES RUN AT ALL? Asked once, up front, with a settings
# file that holds nothing, so the answer is about the tool and not about a case.
STORED_OK=1
STORED_WHY=""
probe_json="$TMPDIR/dataprobe-writer-probe.json"
probe_dat="$TMPDIR/dataprobe-writer-probe.dat"
printf '{"version": [2, 0, 0, 0], "startup": {}}\n' > "$probe_json"
if ! write_settings_dat "$probe_json" "$probe_dat" > "$TMPDIR/dataprobe-writer-probe.log" 2>&1; then
  STORED_OK=0
  STORED_WHY="$(head -3 "$TMPDIR/dataprobe-writer-probe.log")"
fi
rm -f "$probe_dat"

# THE MOD, stamped once. stamp_series says so when it changes the committed
# value, and doing it per case would say so forty-one times.
rm -rf "$TEMPLATE"
mkdir -p "$TEMPLATE"
cp "$PROBE/info.json" "$TEMPLATE/info.json"
stamp_series "$TEMPLATE"

# assemble CASE -- build the whole mod directory for one case.
#
# Both stage files are always written, prelude first, so a case's own file is
# the few lines that ARE the fact. A case naming neither stage file would still
# run: it would be the empty mod, which is the control.
assemble() {
  local name="$1" dir="$MODDIR/${MODNAME}_${MODVER}"
  rm -rf "$MODDIR"
  mkdir -p "$dir"
  cp "$TEMPLATE/info.json" "$dir/info.json"
  cp "$PROBE/mod-list.json" "$MODDIR/mod-list.json"
  cat "$PROBE/prelude.lua" > "$dir/settings.lua"
  if [ -f "$CASES/$name.settings.lua" ]; then cat "$CASES/$name.settings.lua" >> "$dir/settings.lua"; fi
  cat "$PROBE/prelude.lua" > "$dir/data.lua"
  if [ -f "$CASES/$name.data.lua" ]; then cat "$CASES/$name.data.lua" >> "$dir/data.lua"; fi
  # THE ENGINE REWRITES mod-settings.dat after every run with the values it
  # settled on, so a case gets a freshly encoded one rather than the previous
  # case's leftovers, which would be measuring the engine's own rewrite.
  if [ -f "$CASES/$name.stored.json" ]; then
    write_settings_dat "$CASES/$name.stored.json" "$MODDIR/mod-settings.dat" >/dev/null
  fi
}

# first_error LOG -- the first `Error` line, with the engine's leading timestamp
# removed. The timestamp is a duration and would make every line unique.
first_error() {
  grep -m1 ' Error ' "$1" 2>/dev/null | sed 's/^[[:space:]]*[0-9][0-9.]* Error //' || true
}

# engine_seconds LOG -- the last timestamp the engine itself printed.
#
# THE ENGINE'S OWN CLOCK rather than a wall-clock bracket around it: `date +%N`
# is not portable, the number wanted is how long the engine spent, and the
# engine stamps every line with it. An aborted case reads far higher than a
# refusal here, which is itself worth seeing in the table.
engine_seconds() {
  awk '$1 ~ /^[0-9]+\.[0-9]+$/ { t = $1 } END { printf "%.2f", t + 0 }' "$1"
}

# ---- read the expectations -------------------------------------------------
#
# Into arrays first and the engine afterwards: reading a file in the loop that
# runs a subprocess hands that subprocess the file as its stdin.
names=(); outcomes=(); texts=()
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in ''|'#'*) continue ;; esac
  name="${line%%[[:space:]]*}"
  rest="${line#"$name"}"
  rest="${rest#"${rest%%[![:space:]]*}"}"
  outcome="${rest%%[[:space:]]*}"
  text="${rest#"$outcome"}"
  text="${text#"${text%%[![:space:]]*}"}"
  text="${text%"${text##*[![:space:]]}"}"
  case "$outcome" in
    loaded|logged|refused|aborted) ;;
    *) echo "expected.txt: $name names the outcome '$outcome', which is not one of loaded, logged, refused, aborted" >&2; exit 1 ;;
  esac
  # THE TEXT FIELD IS CHECKED HERE OR IT IS A VACUOUS CASE LATER. A `logged`
  # line with no text greps for the empty string, which every log contains; a
  # `loaded` line WITH text has nothing to match it against and would fail for
  # a reason that is about this file rather than about the engine.
  if [ "$outcome" = logged ] && [ -z "$text" ]; then
    echo "expected.txt: $name is 'logged' with no text, which would match any log at all" >&2
    exit 1
  fi
  if [ "$outcome" = loaded ] && [ -n "$text" ]; then
    echo "expected.txt: $name is 'loaded' and carries text; a loaded case asserts its fact in Lua. Use 'logged' for a line the engine wrote" >&2
    exit 1
  fi
  if [ ! -f "$CASES/$name.settings.lua" ] && [ ! -f "$CASES/$name.data.lua" ]; then
    echo "expected.txt: $name names no case file; testdata/dataprobe/cases has neither $name.settings.lua nor $name.data.lua" >&2
    exit 1
  fi
  names+=("$name"); outcomes+=("$outcome"); texts+=("$text")
done < "$EXPECTED"

# EVERY CASE FILE IS NAMED BY expected.txt, from the other side. A case file
# nobody expects anything of is a measurement that runs nowhere, which is the
# same silence as a gate that does not run.
for f in "$CASES"/*.settings.lua "$CASES"/*.data.lua "$CASES"/*.stored.json; do
  [ -e "$f" ] || continue
  base="$(basename "$f")"
  base="${base%.settings.lua}"; base="${base%.data.lua}"; base="${base%.stored.json}"
  found=0
  for n in "${names[@]}"; do [ "$n" = "$base" ] && { found=1; break; }; done
  if [ "$found" = 0 ]; then
    echo "testdata/dataprobe/cases/$(basename "$f") has no line in expected.txt, so it never runs" >&2
    exit 1
  fi
done

echo "running ${#names[@]} cases..."
echo

# ---- run -------------------------------------------------------------------
FAIL=0
JQ_CHECKED=0
rows=()
saidlines=()
for i in "${!names[@]}"; do
  name="${names[$i]}"; want="${outcomes[$i]}"; wanttext="${texts[$i]}"
  log="$LOGS/$name.log"

  # A stored-value case with no writer is NOT RUN, loudly, and the script still
  # fails at the end. Running it without its mod-settings.dat would measure the
  # DEFAULT and report a pass for a fact nothing tested.
  if [ -f "$CASES/$name.stored.json" ] && [ "$STORED_OK" = 0 ]; then
    rows+=("$(printf '%-30s %-8s %-9s %-6s %s' "$name" "$want" "-" "-" "NOT RUN")")
    FAIL=1
    continue
  fi

  assemble "$name"
  rm -f "$DUMP"
  rc=0
  "${TIMEOUT[@]}" "$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --dump-data \
    >"$log" 2>&1 || rc=$?

  secs="$(engine_seconds "$log")"
  err="$(first_error "$log")"

  got=""
  gottext=""
  if [ "$rc" = 0 ] && [ -f "$DUMP" ]; then
    # ONE jq PASS, on the first case that loads: proof that a written dump is a
    # data-raw dump and not a truncated file. Forty-one passes over 13 MB
    # would cost more than the engine runs do and would say the same thing.
    if [ "$JQ_CHECKED" = 0 ]; then
      if jq -e 'has("item") and has("recipe")' "$DUMP" >/dev/null; then
        JQ_CHECKED=1
      else
        echo "the dump at $DUMP does not look like a data-raw dump" >&2
        FAIL=1
      fi
    fi
    # THE LOG DECIDES, NOT $want. An `Error` line in a run that loaded is what
    # `logged` MEANS, so it is read the same way whatever the case expected: a
    # case expecting `loaded` whose engine started writing one fails here and
    # shows the line, which is the whole reason this is not `got=loaded`.
    if [ -n "$err" ]; then
      got=logged
      gottext="$err"
    else
      got=loaded
    fi
  elif [ "$rc" = 124 ] || [ "$rc" = 137 ]; then
    got="timed out"
    gottext="after ${CASE_TIMEOUT}s"
  elif [ -f "$DUMP" ]; then
    # NON-ZERO EXIT AND A DUMP ANYWAY, which is neither outcome: expected.txt
    # says a refusal and an abort both leave no dump, so the absence is checked
    # rather than assumed. Named as its own mismatch instead of being folded
    # into `refused`, where it would pass.
    got="dump written"
    gottext="exit $rc and a dump at ${DUMP#"$USERDIR/"} anyway; refused and aborted both mean no dump"
  elif grep -q 'CrashHandler.cpp' "$log"; then
    got=aborted
    gottext="$err"
  elif [ "$rc" != 0 ]; then
    got=refused
    gottext="$err"
  else
    got="no dump"
    gottext="exit 0 and no $DUMP"
  fi

  # THE CRASH ARTEFACT. An aborted engine dies holding its user directory's
  # .lock, which is a zero-byte advisory file the OS releases with the process
  # -- measured: the very next run starts fine and removes it. Cleaned anyway,
  # because a probe that leaves a lock file behind in a directory it created is
  # a probe whose failures get blamed on the next thing to run.
  #
  # ONLY IN A PRIVATE USER DIRECTORY. Without FACTORIO_USERDIR this is the
  # player's real one, and removing the lock of a Factorio that is genuinely
  # running is a worse thing to do than leaving a zero-byte file behind.
  if [ "$got" = aborted ] && [ -n "${FACTORIO_USERDIR:-}" ]; then rm -f "$USERDIR/.lock"; fi

  result=FAIL
  if [ "$got" = "$want" ]; then
    if [ -z "$wanttext" ]; then
      result=PASS
    elif [ "$want" = logged ]; then
      # THE WHOLE LOG for a `logged` case, the first `Error` line for a refusal
      # or an abort. A logged run has already exited 0 with a dump, so there is
      # no single line the outcome hangs on and the engine may say it anywhere.
      if grep -qF -- "$wanttext" "$log"; then result=PASS; fi
    else
      case "$gottext" in *"$wanttext"*) result=PASS ;; esac
    fi
  fi
  [ "$result" = PASS ] || FAIL=1

  rows+=("$(printf '%-30s %-8s %-9s %5ss %s' "$name" "$want" "$got" "$secs" "$result")")
  if [ -n "$gottext" ]; then saidlines+=("$(printf '%-30s %s' "$name" "$gottext")"); fi
  if [ "$result" = FAIL ]; then
    rows+=("$(printf '%-30s   expected %s %s' "" "$want" "$wanttext")")
    rows+=("$(printf '%-30s   got      %s %s' "" "$got" "$gottext")")
    rows+=("$(printf '%-30s   log      %s' "" "${log#"$ROOT/"}")")
  fi
done

# ---- the mod set actually loaded -------------------------------------------
#
# mod-list.json disables the three bundled DLC, and "it is written in the file"
# is not the same claim as "the engine honoured it headlessly". Every prototype
# in the game is a function of the mod set, so a probe that measured base plus
# Space Age would be measuring a different game from the one its expectations
# were taken on. Asked of the LOGS rather than of the file.
DLC="$(grep -h -o 'Loading mod \(space-age\|quality\|elevated-rails\) [^ ]*' "$LOGS"/*.log 2>/dev/null | sort -u || true)"
BASE_AND_PROBE="$(grep -l "Loading mod $MODNAME .* (data.lua)" "$LOGS"/*.log 2>/dev/null | head -1 || true)"

echo "case                           expected got         engine result"
for r in "${rows[@]}"; do echo "$r"; done
echo

if [ ${#saidlines[@]} -gt 0 ]; then
  echo "--- what the engine said, in full ---"
  for l in "${saidlines[@]}"; do echo "$l"; done
  echo
fi

echo "--- the mod set ---"
if [ -n "$DLC" ]; then
  echo "FAIL: a bundled DLC loaded despite mod-list.json disabling it:" >&2
  echo "$DLC" >&2
  FAIL=1
elif [ -z "$BASE_AND_PROBE" ]; then
  echo "FAIL: no case's log shows $MODNAME loading its data.lua, so nothing here ran in the mod" >&2
  FAIL=1
else
  sed -n 's/.*Loading mod \([^ ]*\) \([^ ]*\) (data\.lua).*/  \1 \2/p' "$BASE_AND_PROBE"
  echo "  (space-age, quality and elevated-rails are disabled in mod-list.json and none of them loaded)"
fi
echo

if [ "$STORED_OK" = 0 ]; then
  cat >&2 <<EOF
NOT RUN: the cases that need a mod-settings.dat could not run, because

    fklua modsettings write --from FILE.json --out mod-settings.dat

did not work. What it said:

$STORED_WHY

Those cases measure what the engine does with a value already STORED for a
setting, which is unreachable without writing the file the engine reads. They
are counted as failures rather than skipped.
EOF
  echo >&2
fi

if [ "$FAIL" != 0 ]; then
  echo "run-dataprobe: FAILED"
  exit 1
fi
echo "run-dataprobe: ok, ${#names[@]} cases, all as expected on Factorio $ENGINE"
