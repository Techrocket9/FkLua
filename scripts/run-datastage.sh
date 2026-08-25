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
# THREE ASSERTIONS, and the order is by strength:
#
#   1. The normalised dump matches a committed golden. This is the real one.
#   2. The GO and RUST guests produce the SAME dump. Two hand-written guest
#      libraries drift, and a census cannot see either of them -- that is the
#      whole reason both were built in one round.
#   3. Two runs of the same guest produce the same dump. A tripwire rather
#      than a discovery: it is true today, and the day it stops being true the
#      data stage has become nondeterministic and every mod built on it is a
#      join refusal waiting to happen.
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

# The CONTROL guest. A data-stage mod still needs one -- `fklua mod` packages a
# control module and Factorio wants a control.lua -- and `hello` is the smallest
# one that is real.
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

# dump_once LANG RUN -> prints the sha256 of the normalised dump
#
# THE MOD NAME IS THE SAME FOR BOTH LANGUAGES, deliberately: a prototype does
# not record which mod defined it, so two runs of one name produce comparable
# dumps where two names would differ in mod-settings-dump.json and nowhere
# useful.
dump_once() {
  local lang="$1" run="$2" out
  rm -rf "$MODDIR"
  mkdir -p "$MODDIR"
  "$ROOT/bin/fklua" mod "$CONTROL" \
    --data-module "$TMPDIR/datastage-$lang.wasm" \
    --name "$MODNAME" --version "$MODVER" --author FkLua \
    --factorio-version "$SERIES" \
    -o "$MODDIR" >"$TMPDIR/datastage-package-$lang.log" 2>&1 ||
    { cat "$TMPDIR/datastage-package-$lang.log" >&2; return 1; }

  rm -f "$USERDIR/script-output/data-raw-dump.json"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --dump-data \
    >"$TMPDIR/datastage-dump-$lang-$run.log" 2>&1 ||
    { cat "$TMPDIR/datastage-dump-$lang-$run.log" >&2; return 1; }

  out="$USERDIR/script-output/data-raw-dump.json"
  [ -f "$out" ] || { echo "no dump at $out" >&2; return 1; }
  "${NORMALISE[@]}" < "$out" | shasum -a 256 | cut -d' ' -f1
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
for lang in $LANGS; do
  echo "--- $lang ---"
  build_data "$lang"
  wasm_bytes=$(wc -c < "$TMPDIR/datastage-$lang.wasm" | tr -d ' ')

  h1="$(dump_once "$lang" 1)"
  h2="$(dump_once "$lang" 2)"
  lua_bytes=$(wc -c < "$MODDIR/${MODNAME}_${MODVER}/fk_data_module.lua" | tr -d ' ')
  proto_count=$(grep -c '"name"' "$USERDIR/script-output/data-raw-dump.json" || true)
  checksum=$(grep -o 'Prototype list checksum: [0-9]*' \
    "$TMPDIR/datastage-dump-$lang-2.log" | tail -1 || true)

  echo "  wasm $wasm_bytes B -> fk_data_module.lua $lua_bytes B"
  echo "  ${checksum:-Prototype list checksum: (not logged)}  <- SMOKE TEST ONLY: blind to field values"
  echo "  sha256 $h1"

  if [ "$h1" != "$h2" ]; then
    echo "  FAIL: two runs of the same guest produced different dumps" >&2
    echo "        $h1" >&2
    echo "        $h2" >&2
    FAIL=1
  else
    echo "  ok: two runs agree (the data stage is deterministic)"
  fi

  # The guest's own log lines. A dump that matched while the guest never ran is
  # the vacuous pass this has to be able to fail on: a data module that
  # exported nothing would leave data.raw untouched and hash to whatever base
  # alone hashes to.
  if grep -q "fkdata example: data stage" "$TMPDIR/datastage-dump-$lang-2.log"; then
    echo "  ok: the guest ran (its log lines are in the dump run)"
  else
    echo "  FAIL: the guest never logged anything, so the dump says nothing" >&2
    FAIL=1
  fi

  if [ -z "$FIRST" ]; then
    FIRST="$h1"
    FIRSTLANG="$lang"
  elif [ "$h1" != "$FIRST" ]; then
    echo "  FAIL: $lang and $FIRSTLANG disagree about what the data stage produced" >&2
    echo "        $FIRSTLANG $FIRST" >&2
    echo "        $lang $h1" >&2
    FAIL=1
  else
    echo "  ok: identical to the $FIRSTLANG guest's dump"
  fi
  echo
done

# THE GOLDEN, per engine. --dump-data was verified on 2.0.77; the flag exists on
# 2.1 and the mechanics are the same, but base's own prototypes move between
# series, so a hash taken on one engine says nothing about the other.
echo "--- golden ---"
mkdir -p "$(dirname "$GOLDEN")"
MODS="$(mod_set "$TMPDIR/datastage-dump-${FIRSTLANG}-2.log")"
echo "mods: $MODS"
WANT=""
WANTMODS=""
if [ -f "$GOLDEN" ]; then
  WANT="$(awk -v e="$ENGINE" '$1 == e { print $2 }' "$GOLDEN")"
  WANTMODS="$(awk -v e="$ENGINE" '$1 == e { $1 = ""; $2 = ""; sub(/^  /, ""); print }' "$GOLDEN")"
fi
if [ -z "$WANT" ]; then
  echo "no golden recorded for Factorio $ENGINE."
  echo "record it with:"
  echo "    echo '$ENGINE  $FIRST  $MODS' >> ${GOLDEN#"$ROOT/"}"
  echo "-- but read the dump first: a golden taken from a broken run is a gate"
  echo "   that certifies the break forever."
elif [ "$WANTMODS" != "$MODS" ]; then
  # NOT A FAILURE OF THE MOD, and saying so is the whole reason the mod set is
  # in the file. A different DLC set is a different game.
  echo "SKIP: the golden for Factorio $ENGINE was taken with a different mod set."
  echo "  golden $WANTMODS"
  echo "  here   $MODS"
  echo "  (dump  $FIRST)"
elif [ "$WANT" != "$FIRST" ]; then
  echo "FAIL: the dump does not match the golden for Factorio $ENGINE" >&2
  echo "  golden $WANT" >&2
  echo "  got    $FIRST" >&2
  echo "The dump is at $USERDIR/script-output/data-raw-dump.json." >&2
  FAIL=1
else
  echo "ok: matches the committed golden for Factorio $ENGINE"
fi

echo
if [ "$FAIL" != 0 ]; then
  echo "run-datastage: FAILED"
  exit 1
fi
echo "run-datastage: ok"
