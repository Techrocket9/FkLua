#!/usr/bin/env bash
# Prove that `mod_changes` reaches a guest, in a real Factorio.
#
# script.on_configuration_changed hands its handler a ConfigurationChangedData:
# which neighbour appeared, disappeared or moved version and from what, whether a
# startup setting moved, whether a migration was applied. The FkLua hook
# dispatched with no arguments at all, so a guest could hear that SOMETHING moved
# and never what.
#
# THE CHEAPEST REAL MOD-SET CHANGE IS A VERSION BUMP, and it is also the one a
# player experiences every time a mod updates. So this packages ONE wasm at TWO
# mod versions, creates a save with the first, and loads it with the second: the
# engine raises the hook naming this mod with an old_version and a new_version,
# which no host-side stub can produce because the engine is what compares the two
# saves' mod lists.
#
# The WASM IS IDENTICAL between the two packages, deliberately. A build stamp
# that moved would take the rebuild path -- fk_migrate, a declined heap -- and
# this is about the OTHER thing this hook reports, which is the one that had no
# expression at all. Only info.json's version differs.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

MODNAME=fk-confchanged
TMPDIR="$ROOT/testdata/tmp"
MODDIR="$TMPDIR/confchanged-mods"
MAP="$TMPDIR/confchanged-map.zip"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO" >&2
                        echo "set FACTORIO_BIN" >&2; exit 1; }
command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
export PATH="/opt/homebrew/opt/binaryen/bin:$PATH"
command -v wasm-opt >/dev/null || { echo "wasm-opt is not installed: brew install binaryen" >&2; exit 1; }

SERIES="$(factorio_series)"

# A PRIVATE WRITE-DATA DIRECTORY, so this can run while a Factorio is already
# open. Factorio LOCKS its user directory and a second process pointed at the
# same one dies at startup, which reads as a broken gate.
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

mkdir -p "$TMPDIR"
rm -rf "$MODDIR" "$MAP"
mkdir -p "$MODDIR"

# BUILD THE COMPILER EVERY RUN. runtime/lua/fk_mod.lua is EMBEDDED in the binary
# and this gate is about a change to it; a stale bin/fklua would package the
# previous shim and report it as the current one.
echo "==> building the compiler"
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"

echo "==> building the guest"
(cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
  -gc=leaking -opt=2 -o "$TMPDIR/confchanged.wasm" ./examples/confchanged)

package_at() { # package_at VERSION
  rm -rf "$MODDIR"
  mkdir -p "$MODDIR"
  "$ROOT/bin/fklua" mod "$TMPDIR/confchanged.wasm" \
    --factorio-version "$SERIES" \
    --name "$MODNAME" --version "$1" --author FkLua \
    --description "FkLua configuration-changed payload fixture" \
    -o "$MODDIR" >"$TMPDIR/confchanged-package-$1.log" 2>&1 ||
    { cat "$TMPDIR/confchanged-package-$1.log" >&2; return 1; }
  cat > "$MODDIR/mod-list.json" <<JSON
{
  "mods": [
    { "name": "base", "enabled": true },
    { "name": "$MODNAME", "enabled": true }
  ]
}
JSON
}

echo "==> creating the save at 0.1.0"
package_at 0.1.0
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --create "$MAP" --disable-audio \
  >"$TMPDIR/confchanged-create.log" 2>&1 \
  || { echo "map creation failed; see $TMPDIR/confchanged-create.log" >&2
       tail -40 "$TMPDIR/confchanged-create.log" >&2; exit 1; }

echo "==> loading it at 0.2.0"
package_at 0.2.0
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" \
            --benchmark "$MAP" --benchmark-ticks 10 --benchmark-runs 1 \
            --disable-audio >"$TMPDIR/confchanged-run.log" 2>&1 \
  || { echo "run failed; see $TMPDIR/confchanged-run.log" >&2
       tail -40 "$TMPDIR/confchanged-run.log" >&2; exit 1; }

LOG="$TMPDIR/confchanged-run.log"
echo "==> guest output"
grep -E "confchanged:" "$LOG" || true

fail=0
check() {
  if grep -qF -- "$2" "$LOG"; then printf '  ok   %s\n' "$1"
  else printf '  FAIL %s\n      wanted: %s\n' "$1" "$2" >&2; fail=1; fi
}
checkre() {
  if grep -qE -- "$2" "$LOG"; then printf '  ok   %s\n' "$1"
  else printf '  FAIL %s\n      wanted (regex): %s\n' "$1" "$2" >&2; fail=1; fi
}

echo "==> assertions"
# THE PAYLOAD ARRIVED AND CARRIES ONE CHANGE: this mod's own version move. A
# hook that was entered with no argument would log changes=0 and no `mod` line.
check "the hook was entered with a payload"   "confchanged: told changes=1"
check "...naming THIS mod and both versions"  "confchanged: mod $MODNAME old=0.1.0 new=0.2.0"
# The rest of the struct crossed too, which is what says the whole layout is
# right rather than the one field the assertion above reads.
check "...and the two boolean flags"          "startup=0 migrated=0"
# THE NESTED DICTIONARY, and this is the field worth having a separate line for.
# `migrations` is dictionary[IDType -> dictionary[string -> string]] -- a
# dictionary OF a dictionary, which is a shape the generators only learned to
# express in the nested-container round -- and the engine fills it with base's
# own prototype migrations. Measured here at 42 on 2.0.77; the COUNT is base's
# and moves with it, so what is asserted is that it is not zero. A zero would
# mean the container crossed empty, which is exactly what a layout that got the
# nesting wrong would produce.
checkre "...and the nested migrations dictionary" "confchanged: told .* migrations=[1-9]"
# ...AND THE GUEST IS STILL RUNNING AFTERWARDS, so nothing about the payload
# took the dispatch down.
check "the guest keeps running"               "confchanged: still running at tick 3"

if [ "$fail" -ne 0 ]; then
  echo "FAILED; see $LOG" >&2
  exit 1
fi
echo "==> ok"
