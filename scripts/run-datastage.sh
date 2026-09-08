#!/usr/bin/env bash
# Build a data-stage guest, package it, and run its SETTINGS and DATA stages
# inside a real Factorio -- then hash what came out.
#
# This is the strongest gate the data-stage feature has, and it is the only one
# that is Factorio. lua52f models the sandbox and the end-to-end test drives the
# generated stage files against a stand-in `data` table, but neither is the
# engine: the stage sequence, `require` resolution inside a mod, `util`,
# `data:extend`'s own validation and the prototype loader are all outside what
# the oracle can speak to.
#
# `--dump-data` is what makes it cheap. It runs the settings and data stages,
# writes script-output/data-raw-dump.json, and STOPS -- it never reaches
# control.lua, so this is a pure data-stage instrument and it costs about two
# and a half seconds per run.
#
# TWO ROWS PER LANGUAGE, AND THE SECOND ONE IS A PLAYER'S CHOICE. The DEFAULT
# row is every setting left where the guest declared it, which is the load a
# player who never opens the settings screen gets. The FLIPPED row is the same
# mod with testdata/datastage/flipped.json written into the mod directory as
# mod-settings.dat by `fklua modsettings write`: the example guests' own
# fkd-enabled turned off, which the settings stage stores and the data stage
# reads back through env(3) into the marker technology's `enabled` field.
#
# Until that row existed this gate could only ever run the stages on DECLARED
# values, and "the settings stage stores what a player chose and the data stage
# reads it back" was the one thing about the feature nothing measured. Nothing
# but the engine can measure it: a stand-in has no stored value to read.
#
# THE WRITER IS FKLUA'S OWN, and that is deliberate rather than convenient.
# This script already builds bin/fklua every run (see below), so `fklua
# modsettings write` costs nothing extra; a second tool would be a second thing
# to build, a second implementation of the binary format to keep in step, and a
# second answer the day the two disagreed. It is also the whole reason the
# command exists in the compiler: every guest library on the channel gets this
# row by writing its own JSON.
#
# SEVEN ASSERTIONS, and the order is by strength:
#
#   1. The normalised dumps match a committed golden, for BOTH rows. This is
#      the real one.
#   2. The GO and RUST guests produce the SAME dumps, for both rows. Two
#      hand-written guest libraries drift, and a census cannot see either of
#      them -- that is the whole reason both were built in one round.
#   3. Two runs of the same guest produce the same dumps. A tripwire rather
#      than a discovery: it is true today, and the day it stops being true the
#      data stage has become nondeterministic and every mod built on it is a
#      join refusal waiting to happen.
#   4. The FLIPPED dump DIFFERS from the default one. A settings file the
#      engine ignored, one this script failed to install, or a writer that
#      wrote the declared default would otherwise produce a second row
#      identical to the first, and every check above would still pass.
#   5. Each row logs the value it ran on by name: `startup fkd-enabled is true`
#      on the default row and `is false` on the flipped one. A hash can only
#      say "different"; this names the cause.
#   6. The mod-settings.dat the engine REWROTE after the flipped run still
#      carries false, read back with `fklua modsettings read`. The engine
#      rewrites the file with the values it settled on and resets anything out
#      of range or off a dropdown's list, so this is asserted rather than
#      assumed.
#   7. The guest's own log lines are in each run, so a dump that matched while
#      the guest never ran cannot pass.
#
# NORMALISATION IS NOT OPTIONAL. Key order in the dump is INSERTION order, so
# two mods differing only in the order they extend produce dumps that differ at
# a byte and describe the same game. `jq -S` sorts every object; measured, it
# removes an ordering difference completely and preserves a real field-value
# change (stack_size 1 -> 42 survives it).
#
# THE ENGINE'S OWN `Prototype list checksum` IS A SMOKE TEST AND NOT THE GATE.
# It is order-insensitive, which sounds like what is wanted, and it is BLIND TO
# FIELD VALUES -- measured: it is unchanged when a prototype's stack_size goes
# from 1 to 42. Quoting it as an equivalence proof would be a gate that cannot
# fail on the defect class this feature is most likely to produce.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

# Which languages to run. Both by default, because the mirror is the point.
LANGS="${LANGS:-go rust}"
MODNAME=fkd-example
MODVER=0.1.0

TMPDIR="$ROOT/testdata/tmp"
MODDIR="$TMPDIR/datastage-mods"
GOLDEN="$ROOT/testdata/datastage/dump-sha256.txt"
FLIPPED_JSON="$ROOT/testdata/datastage/flipped.json"
FLIPPED_DAT="$TMPDIR/datastage-flipped.dat"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO" >&2; echo "set FACTORIO_BIN" >&2; exit 1; }
SERIES="$(factorio_series)"
ENGINE="$(factorio_version_triple)"

# THE NORMALISER. jq is the one this was measured with; python3's json module
# with sort_keys is the fallback, and it is a fallback rather than the default
# because it re-renders every float and a float that round-trips differently
# would move the hash for a reason that has nothing to do with the mod.
if command -v jq >/dev/null 2>&1; then
  NORMALISE=(jq -S .)
  NORMALISER="jq $(jq --version)"
elif command -v python3 >/dev/null 2>&1; then
  NORMALISE=(python3 -c 'import json,sys; json.dump(json.load(sys.stdin), sys.stdout, sort_keys=True, separators=(",",":"))')
  NORMALISER="python3 json.dump(sort_keys=True)"
  echo "NOTE: jq is not installed, so the dump is normalised with python3." >&2
  echo "      The committed golden was taken with jq; if the hash differs," >&2
  echo "      install jq before concluding anything about the mod." >&2
else
  echo "neither jq nor python3 is installed, and the dump has to be normalised" >&2
  exit 1
fi

# A PRIVATE WRITE-DATA DIRECTORY, so this can run while a Factorio is already
# open. Factorio LOCKS its user directory and a second process pointed at the
# same one dies at startup, which reads as a broken gate rather than as two
# copies of the game. It is also where --dump-data writes: the dump lands in
# $USERDIR/script-output, so a private one keeps this out of a real install.
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

echo "=== data stage in a real Factorio ==="
echo "engine:     $ENGINE (info.json will declare $SERIES)"
echo "normaliser: $NORMALISER"
echo

mkdir -p "$TMPDIR"
export PATH="/opt/homebrew/opt/binaryen/bin:$PATH"

# BUILD THE COMPILER, EVERY RUN, AND THIS IS NOT HOUSEKEEPING.
#
# runtime/lua/fk_data.lua is EMBEDDED in the binary, so a stale bin/fklua
# packages a stale shim -- and the failure is a dump that describes the last
# build rather than this one. That cost a red proof in this feature's own round:
# a defect injected into the shim, reverted, and then measured again against a
# binary nobody had rebuilt, which reported the reverted defect as still
# present. Identical output from a changed input is a bug in the harness until
# proven otherwise; agents/testing.md has the longer version, twice.
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"

# THE FLIPPED ROW'S SETTINGS FILE, written by the compiler that was just built.
# There is no CLI for a mod-settings.dat outside this one: the engine writes it
# and reads it back, and a headless gate that wants a player's chosen value in
# front of the settings stage has to write those bytes itself. `write` decodes
# what it encoded before writing, so a row edited into something that does not
# round trip is a sentence naming the byte here rather than a Factorio that
# refuses to start twenty seconds later.
echo "writing the flipped row's mod-settings.dat..."
[ -f "$FLIPPED_JSON" ] || { echo "no flipped settings at $FLIPPED_JSON" >&2; exit 1; }
"$ROOT/bin/fklua" modsettings write --from "$FLIPPED_JSON" --out "$FLIPPED_DAT" ||
  { echo "the flipped settings file did not encode" >&2; exit 1; }

# The CONTROL guest, and this gate KEEPS ONE ON PURPOSE now that it does not
# have to.
#
# Both halves of the old reason are gone. `fklua mod`'s positional is optional
# whenever the mod has a data module, and Factorio never wanted a control.lua
# either: info.json is the only file it insists on, and a data-stage-only mod
# loads -- measured downstream by a harness that stages one under four different
# names. So the control guest is no longer a tax here; it is what makes this the
# WIDER arm. A dump taken with one present proves the data stage runs in the mod
# shape a real mod has, where a dump taken without one would leave "does a
# control module change what the data stage sees" untested, and that is the
# question two modules in one mod exists to answer.
#
# `hello` is the smallest control guest that is real. Packaging the same data
# module WITHOUT it is the natural second arm and is deliberately not taken: it
# is a second engine run and a second golden for a difference this instrument
# cannot see, since --dump-data never reaches control.lua at all.
CONTROL="$TMPDIR/datastage-control.wasm"
echo "building the control guest..."
(cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
  -gc=leaking -opt=2 -o "$CONTROL" ./examples/hello)

# build_data LANG -> $TMPDIR/datastage-$LANG.wasm
build_data() {
  case "$1" in
    go)
      (cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
        -gc=leaking -opt=2 -o "$TMPDIR/datastage-go.wasm" ./examples/datastage)
      ;;
    rust)
      (cd "$ROOT/guest/rust" && cargo build --release \
        --target wasm32-unknown-unknown -p datastage >/dev/null)
      cp "$ROOT/guest/rust/target/wasm32-unknown-unknown/release/datastage.wasm" \
        "$TMPDIR/datastage-rust.wasm"
      ;;
    *) echo "unknown language: $1" >&2; return 1 ;;
  esac
}

# dump_once LANG RUN [SETTINGS] -> prints "<data sha256> <settings sha256>",
# both over normalised dumps, and keeps the run's own dump and the mod
# directory's mod-settings.dat under $TMPDIR so a later assertion can name a
# particular row instead of reading whatever the last run happened to leave in
# script-output.
#
# SETTINGS, when given, is a mod-settings.dat installed before the run.
# MEASURED: the engine reads MODS/mod-settings.dat, beside the mod folders
# rather than inside one, and it REWRITES that file after the run with the
# values it settled on. The rewrite is what the flipped row's read-back
# assertion reads, and this directory is torn down at the top of the next run,
# so it is kept here or it is gone.
#
# THE SETTINGS DUMP IS HASHED TOO, and it took a gap to learn why: setting
# prototypes live in the settings stage's own Lua state and NEVER reach
# data-raw-dump.json, so a golden over the data dump alone is green for a
# guest whose fk_settings hook silently did nothing -- the recorded "a gate
# that cannot fail" shape, met at the settings stage.
#
# THE MOD NAME IS THE SAME FOR BOTH LANGUAGES, deliberately: a prototype does
# not record which mod defined it, so two runs of one name produce comparable
# dumps where two names would put a difference in mod-settings-dump.json that
# says nothing useful.
dump_once() {
  local lang="$1" run="$2" settings="${3:-}" out sout dhash shash
  rm -rf "$MODDIR"
  mkdir -p "$MODDIR"
  "$ROOT/bin/fklua" mod "$CONTROL" \
    --data-module "$TMPDIR/datastage-$lang.wasm" \
    --name "$MODNAME" --version "$MODVER" --author FkLua \
    --factorio-version "$SERIES" \
    -o "$MODDIR" >"$TMPDIR/datastage-package-$lang.log" 2>&1 ||
    { cat "$TMPDIR/datastage-package-$lang.log" >&2; return 1; }

  if [ -n "$settings" ]; then
    cp "$settings" "$MODDIR/mod-settings.dat" ||
      { echo "could not install $settings into $MODDIR" >&2; return 1; }
  fi

  rm -f "$USERDIR/script-output/data-raw-dump.json" \
        "$USERDIR/script-output/mod-settings-dump.json"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --dump-data \
    >"$TMPDIR/datastage-dump-$lang-$run.log" 2>&1 ||
    { cat "$TMPDIR/datastage-dump-$lang-$run.log" >&2; return 1; }

  out="$USERDIR/script-output/data-raw-dump.json"
  sout="$USERDIR/script-output/mod-settings-dump.json"
  [ -f "$out" ] || { echo "no dump at $out" >&2; return 1; }
  [ -f "$sout" ] || { echo "no settings dump at $sout" >&2; return 1; }
  cp "$out" "$TMPDIR/datastage-raw-$lang-$run.json" || return 1

  # The engine's own mod-settings.dat, kept whether one was installed or not: a
  # run with no file gets one WRITTEN from the declared defaults, and a run with
  # one gets it REWRITTEN. Both are the engine's bytes and both are read back.
  [ -f "$MODDIR/mod-settings.dat" ] ||
    { echo "the engine wrote no mod-settings.dat into $MODDIR" >&2; return 1; }
  cp "$MODDIR/mod-settings.dat" "$TMPDIR/datastage-settings-$lang-$run.dat" || return 1

  # ONE HASH PER LINE, AND EACH LINE CHECKS ITS OWN STATUS. Inside `echo`'s
  # argument list the status of a command substitution is echo's, not the
  # pipeline's, so a normaliser that died handed back the hash of EMPTY INPUT --
  # e3b0c442..., stable, identical for both dumps, both runs and both languages,
  # which every comparison in this script agrees with (measured: only the
  # committed golden noticed).
  #
  # AND `set -e` IS NOT THE FIX, which is the part worth writing down: this
  # function's output is captured with `h1="$(dump_once ...)"`, and inside a
  # command substitution bash does not honour errexit at all -- measured on bash
  # 5.3.15, a bare `false` in a function called that way is stepped straight
  # over, while the same function called plainly aborts. So the status is tested
  # here explicitly, the way every other step of this function already does.
  dhash="$("${NORMALISE[@]}" < "$out" | shasum -a 256 | cut -d' ' -f1)" ||
    { echo "the normaliser failed on $out" >&2; return 1; }
  shash="$("${NORMALISE[@]}" < "$sout" | shasum -a 256 | cut -d' ' -f1)" ||
    { echo "the normaliser failed on $sout" >&2; return 1; }
  echo "$dhash $shash"
}

# mod_set LOG -- the data-stage mod set, as one comparable string.
#
# THE DUMP IS A FUNCTION OF EVERY MOD THAT RAN, not of this one. Factorio's
# bundled DLC data (elevated-rails, quality, space-age) loads whatever
# --mod-directory says, so a machine that owns different DLC produces a
# different dump for a mod that is perfectly fine -- and a golden that could
# only say "does not match" would be a gate whose failure means nothing. Record
# it beside the hash, so a mismatch names its own cause.
mod_set() {
  sed -n 's/.*Loading mod \([^ ]*\) \([^ ]*\) (data\.lua).*/\1@\2/p' "$1" |
    sort | tr '\n' ' ' | sed 's/ $//'
}

FAIL=0
FIRST=""
FIRSTFLIPPED=""
for lang in $LANGS; do
  echo "--- $lang ---"
  build_data "$lang"
  wasm_bytes=$(wc -c < "$TMPDIR/datastage-$lang.wasm" | tr -d ' ')

  h1="$(dump_once "$lang" 1)"
  h2="$(dump_once "$lang" 2)"
  # THE FLIPPED ROW RUNS LAST AND ONCE. Determinism is a property of the data
  # stage rather than of a particular settings file, so the second DEFAULT run
  # is what proves it and a second flipped run would buy nothing but two and a
  # half more seconds.
  hf="$(dump_once "$lang" flipped "$FLIPPED_DAT")"
  lua_bytes=$(wc -c < "$MODDIR/${MODNAME}_${MODVER}/fk_data_module.lua" | tr -d ' ')
  checksum=$(grep -o 'Prototype list checksum: [0-9]*' \
    "$TMPDIR/datastage-dump-$lang-2.log" | tail -1 || true)

  echo "  wasm $wasm_bytes B -> fk_data_module.lua $lua_bytes B"
  echo "  ${checksum:-Prototype list checksum: (not logged)}  <- SMOKE TEST ONLY: blind to field values"
  echo "  sha256 default (data, settings) $h1"
  echo "  sha256 flipped (data, settings) $hf"

  if [ "$h1" != "$h2" ]; then
    echo "  FAIL: two runs of the same guest produced different dumps" >&2
    echo "        $h1" >&2
    echo "        $h2" >&2
    FAIL=1
  else
    echo "  ok: two runs agree (the data stage is deterministic)"
  fi

  # A SETTINGS FILE THE ENGINE IGNORED, one this script failed to install, or a
  # writer that wrote the DECLARED default would leave the flipped row identical
  # to the default one -- and every other check here would still pass: the two
  # languages would agree, both runs would agree, and the golden would hold two
  # identical hashes proving nothing about a player's choice.
  if [ "$hf" = "$h1" ]; then
    echo "  FAIL: the flipped row produced the same dump as the default one," >&2
    echo "        so mod-settings.dat changed nothing" >&2
    echo "        $FLIPPED_DAT was installed into $MODDIR" >&2
    FAIL=1
  else
    echo "  ok: the flipped settings file moved the dump"
  fi

  # The guest's own log lines, IN EVERY RUN. A dump that matched while the guest
  # never ran is the vacuous pass this has to be able to fail on: a data module
  # that exported nothing would leave data.raw untouched and hash to whatever
  # base alone hashes to. Checking one of the three runs would leave the other
  # two carrying that pass, and the flipped run is the one with a settings file
  # in it -- the run where a guest is likeliest to die before it logs.
  guest_ran=1
  for run in 1 2 flipped; do
    if ! grep -q "fkdata example: data stage" "$TMPDIR/datastage-dump-$lang-$run.log"; then
      echo "  FAIL: the guest logged nothing in run $run, so that dump says nothing" >&2
      echo "        (the log is $TMPDIR/datastage-dump-$lang-$run.log)" >&2
      guest_ran=0
      FAIL=1
    fi
  done
  if [ "$guest_ran" = 1 ]; then
    echo "  ok: the guest ran in all three runs (its log lines are in each)"
  fi

  # THE SETTINGS -> DATA ROUND TRIP, asserted in game by name, ON BOTH ROWS: the
  # setting fk_settings declared came back through env(3) at the data stage,
  # with its DECLARED value on the default row and with the PLAYER'S on the
  # flipped one. The dump hashes cover the same fact through the marker
  # technology's enabled field, but a hash can only say "different"; these name
  # the cause, and the pair is what tells "the value moved" from "something else
  # moved".
  if grep -q "fkdata example: startup fkd-enabled is true" \
    "$TMPDIR/datastage-dump-$lang-2.log"; then
    echo "  ok: the settings stage's own setting read back at the data stage"
  else
    echo "  FAIL: the startup setting never came back at the data stage" >&2
    FAIL=1
  fi
  if grep -q "fkdata example: startup fkd-enabled is false" \
    "$TMPDIR/datastage-dump-$lang-flipped.log"; then
    echo "  ok: the flipped row's stored value reached the data stage"
  else
    echo "  FAIL: the flipped row's data stage did not read fkd-enabled as false" >&2
    echo "        (the log is $TMPDIR/datastage-dump-$lang-flipped.log)" >&2
    FAIL=1
  fi

  # WHAT THE ENGINE SETTLED ON, read back rather than assumed. The engine
  # rewrites mod-settings.dat after the run with the values it kept, and it does
  # NOT keep everything: a number outside a setting's range and a dropdown value
  # off its list are both RESET to the default (measured). So a row that the
  # engine silently discarded would still be in the file this script wrote, and
  # only the file the engine wrote can say what ran.
  if "$ROOT/bin/fklua" modsettings read "$TMPDIR/datastage-settings-$lang-flipped.dat" \
    >"$TMPDIR/datastage-settings-$lang-flipped.json" 2>"$TMPDIR/datastage-settings-$lang-flipped.err"; then
    if grep -q '"fkd-enabled": false' "$TMPDIR/datastage-settings-$lang-flipped.json"; then
      echo "  ok: the engine's own rewrite still carries fkd-enabled false"
    else
      echo "  FAIL: the mod-settings.dat the engine rewrote does not carry fkd-enabled false" >&2
      sed 's/^/        /' "$TMPDIR/datastage-settings-$lang-flipped.json" >&2
      FAIL=1
    fi
  else
    echo "  FAIL: the mod-settings.dat the engine rewrote does not read back" >&2
    sed 's/^/        /' "$TMPDIR/datastage-settings-$lang-flipped.err" >&2
    FAIL=1
  fi

  if [ -z "$FIRST" ]; then
    FIRST="$h1"
    FIRSTFLIPPED="$hf"
    FIRSTLANG="$lang"
  else
    if [ "$h1" != "$FIRST" ]; then
      echo "  FAIL: $lang and $FIRSTLANG disagree about the default row" >&2
      echo "        $FIRSTLANG $FIRST" >&2
      echo "        $lang $h1" >&2
      FAIL=1
    else
      echo "  ok: the default row is identical to the $FIRSTLANG guest's"
    fi
    if [ "$hf" != "$FIRSTFLIPPED" ]; then
      echo "  FAIL: $lang and $FIRSTLANG disagree about the flipped row" >&2
      echo "        $FIRSTLANG $FIRSTFLIPPED" >&2
      echo "        $lang $hf" >&2
      FAIL=1
    else
      echo "  ok: the flipped row is identical to the $FIRSTLANG guest's"
    fi
  fi
  echo
done

# THE GOLDEN, per engine AND PER ROW. --dump-data was verified on 2.0.77; the
# flag exists on 2.1 and the mechanics are the same, but base's own prototypes
# move between series, so a hash taken on one engine says nothing about the
# other. The ROW tag is the second key because one file now describes two loads
# of the same mod: the settings as the guest declares them, and the settings as
# a player chose them.
echo "--- golden ---"
mkdir -p "$(dirname "$GOLDEN")"
MODS="$(mod_set "$TMPDIR/datastage-dump-${FIRSTLANG}-2.log")"
echo "mods: $MODS"

# compare_row ROW HASHES KEPT -- one tagged golden line against one row.
compare_row() {
  local row="$1" hashes="$2" kept="$3" want wantmods
  want=""
  wantmods=""
  if [ -f "$GOLDEN" ]; then
    want="$(awk -v e="$ENGINE" -v r="$row" '$1 == e && $2 == r { print $3 " " $4 }' "$GOLDEN")"
    wantmods="$(awk -v e="$ENGINE" -v r="$row" \
      '$1 == e && $2 == r { $1 = ""; $2 = ""; $3 = ""; $4 = ""; sub(/^ +/, ""); print }' "$GOLDEN")"
  fi
  if [ -z "$want" ]; then
    # THE ENGINE HAS A LINE AND THIS ROW IS NOT IN IT. That is a half-recorded
    # golden rather than an unrecorded engine, and it is exactly what a file
    # written before the row tag existed looks like: reading it as "record it"
    # would leave one of the two rows silently unchecked forever.
    echo "FAIL: the golden has a line for Factorio $ENGINE but no $row row" >&2
    echo "  re-record BOTH rows for this engine:" >&2
    echo "    $ENGINE default $FIRST $MODS" >&2
    echo "    $ENGINE flipped $FIRSTFLIPPED $MODS" >&2
    FAIL=1
    return
  fi
  if [ "$wantmods" != "$MODS" ]; then
    # NOT A FAILURE OF THE MOD, and saying so is the whole reason the mod set is
    # in the file. A different DLC set is a different game.
    echo "SKIP: the $row golden for Factorio $ENGINE was taken with a different mod set."
    echo "  golden $wantmods"
    echo "  here   $MODS"
    echo "  (dump  $hashes)"
    return
  fi
  if [ "$want" != "$hashes" ]; then
    echo "FAIL: the $row dumps do not match the golden for Factorio $ENGINE" >&2
    echo "  golden $want" >&2
    echo "  got    $hashes" >&2
    echo "  the dump is at $kept" >&2
    FAIL=1
    return
  fi
  echo "ok: the $row dumps match the committed golden for Factorio $ENGINE"
}

# A missing line reads as "record it" rather than as a failure, and it names
# BOTH lines: a golden with one of the two rows would leave the other silently
# unchecked, which is the shape this repository keeps meeting.
if [ ! -f "$GOLDEN" ] ||
   [ -z "$(awk -v e="$ENGINE" '$1 == e { print }' "$GOLDEN")" ]; then
  echo "no golden recorded for Factorio $ENGINE."
  echo "record it with:"
  echo "    cat >> ${GOLDEN#"$ROOT/"} <<'EOF'"
  echo "$ENGINE default $FIRST $MODS"
  echo "$ENGINE flipped $FIRSTFLIPPED $MODS"
  echo "EOF"
  echo "-- but read the dumps first: a golden taken from a broken run is a gate"
  echo "   that certifies the break forever."
else
  compare_row default "$FIRST" "$TMPDIR/datastage-raw-$FIRSTLANG-2.json"
  compare_row flipped "$FIRSTFLIPPED" "$TMPDIR/datastage-raw-$FIRSTLANG-flipped.json"
fi

echo
if [ "$FAIL" != 0 ]; then
  echo "run-datastage: FAILED"
  exit 1
fi
echo "run-datastage: ok"
