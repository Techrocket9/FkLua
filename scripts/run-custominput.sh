#!/usr/bin/env bash
# Run a NAME-ADDRESSED subscription inside a real Factorio, headlessly.
#
# A custom input is Factorio's keybind: a mod declares a custom-input prototype
# at the data stage and subscribes to it by that prototype's own NAME. The event
# has no defines.events entry at all -- measured on this engine, the table holds
# 233 keys and CustomInputEvent is not one of them -- so `fk.subscribe`'s numeric
# form could never reach it, and a whole genre of mod was unwritable.
#
# THIS IS THE ONLY GATE THAT CAN SEE THE SUCCESS PATH. `script.on_event` accepts
# a string only when a custom-input prototype of that name is LOADED, so the
# fixture is a pair: a control guest that subscribes and a DATA guest that
# defines the prototypes. Nothing host-side can produce that, because the
# prototype loader is the engine.
#
# WHAT IT CANNOT DO IS PRESS A KEY, and it cannot fake one either. Measured on
# 2.0.77 with a bare Lua mod:
#
#   script.on_event("<known custom input>", f)    ok
#   script.on_event("<unknown name>", f)          RAISES "Unknown event name: ..."
#   defines.events.CustomInputEvent               nil, over 233 other keys
#   script.get_event_id("<known custom input>")   a real number (218)
#   script.raise_event("<known custom input>", d) REFUSED, "... can't be raised
#                                                 through script."
#
# So the dispatch half is proven host-side, through the verbatim fk_mod.lua
# against an engine-shaped stub (TestASubscriptionByNameRegistersAndDispatches),
# and what this proves is everything up to the keypress: the registration is
# accepted by the real engine, a typo is refused as a STATUS with the engine's
# own words rather than as a mod that will not load, the unnamed form is
# diagnosed truthfully, and the raise refusal comes back through fk.LastError
# verbatim -- a tripwire, so the day Factorio starts allowing that raise this
# gate says so instead of passing over a path that has become testable for real.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"

MODNAME=fk-custominput
TMPDIR="$ROOT/testdata/tmp"
MODDIR="$TMPDIR/custominput-mods"
MAP="$TMPDIR/custominput-map.zip"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO" >&2
                        echo "set FACTORIO_BIN" >&2; exit 1; }
command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
export PATH="/opt/homebrew/opt/binaryen/bin:$PATH"
command -v wasm-opt >/dev/null || { echo "wasm-opt is not installed: brew install binaryen" >&2; exit 1; }

# The INSTALLED engine's series. info.json's factorio_version is a claim about
# the ENGINE and the API pin is a claim about the DESCRIPTION, and the two
# default apart: a 2.1 engine refuses a mod declaring 2.0 at game start.
SERIES="$(factorio_series)"

# A PRIVATE WRITE-DATA DIRECTORY, so this can run while a Factorio is already
# open. Factorio LOCKS its user directory and a second process pointed at the
# same one dies at startup, which reads as a broken gate rather than as two
# copies of the game.
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

# BUILD THE COMPILER EVERY RUN. runtime/lua/fk_mod.lua is EMBEDDED in the binary,
# and this gate is about a change to that file -- a stale bin/fklua would package
# the previous shim and report it as the current one. Identical output from a
# changed input is a bug in the harness until proven otherwise.
echo "==> building the compiler"
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"

echo "==> building the guests"
(cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
  -gc=leaking -opt=2 -o "$TMPDIR/custominput.wasm" ./examples/custominput)
(cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
  -gc=leaking -opt=2 -o "$TMPDIR/custominputdata.wasm" ./examples/custominputdata)

echo "==> packaging"
"$ROOT/bin/fklua" mod "$TMPDIR/custominput.wasm" \
  --data-module "$TMPDIR/custominputdata.wasm" \
  --factorio-version "$SERIES" \
  --name "$MODNAME" --version 0.1.0 --author FkLua \
  --description "FkLua custom-input subscription fixture" \
  -o "$MODDIR"

cat > "$MODDIR/mod-list.json" <<JSON
{
  "mods": [
    { "name": "base", "enabled": true },
    { "name": "$MODNAME", "enabled": true }
  ]
}
JSON

echo "==> creating throwaway map"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --create "$MAP" --disable-audio \
  >"$TMPDIR/custominput-create.log" 2>&1 \
  || { echo "map creation failed; see $TMPDIR/custominput-create.log" >&2
       tail -40 "$TMPDIR/custominput-create.log" >&2; exit 1; }

LOG="$TMPDIR/custominput-create.log"
echo "==> guest output"
grep -E "custominput|custominputdata|fklua: " "$LOG" || true

fail=0
check() { # check DESCRIPTION PATTERN
  if grep -qF -- "$2" "$LOG"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s\n      wanted: %s\n' "$1" "$2" >&2
    fail=1
  fi
}
reject() { # reject DESCRIPTION PATTERN
  if grep -qF -- "$2" "$LOG"; then
    printf '  FAIL %s\n      found: %s\n' "$1" "$2" >&2
    fail=1
  else
    printf '  ok   %s\n' "$1"
  fi
}

echo "==> assertions"
# THE SUCCESS PATH, which is the whole reason this gate exists. Status 0 from a
# name-addressed subscription means script.on_event took the string.
check "a named subscription is accepted"        "custominput: primary st=0"
check "...and so is the masked named form"      "custominput: second st=0"
# THE TYPO PATH. ERR_NO_MEMBER is 3, and the engine's own sentence is in the log
# beside it rather than replaced by one of ours.
check "a name no prototype has is a status"     "custominput: absent st=3"
check "...carrying the engine's own words"      "Unknown event name: fkci-no-such-input"
check "...diagnosed by fklua as a refusal"      "script.on_event refused the event name"
# THE TRAP. The numeric form on a name-addressed event cannot work, and what it
# must not do is claim this Factorio has no such event.
check "the unnamed form is refused"             "custominput: unnamed st=3"
check "...and diagnosed truthfully"             "could not resolve defines.events.CustomInputEvent"
reject "...with no false sentence"              "this Factorio has no event CustomInputEvent"
# THE TRIPWIRE. A custom input has a real numeric event id and still cannot be
# raised from script. If either half of that changes, this gate is what says so.
check "a custom input has a real event id"      "custominput: get_event_id="
check "...and cannot be raised from script"     "custominput: raise ok=false"
check "...refused in the engine's own words"    "can't be raised through script"

if [ "$fail" -ne 0 ]; then
  echo "FAILED; see $LOG" >&2
  exit 1
fi
echo "==> ok"
