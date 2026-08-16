#!/usr/bin/env bash
# The two-mod fkipc demo: a Go mod and a Rust mod in ONE Factorio, both steered
# live from one companion process.
#
# scripts/run-ipc.sh is the standing protocol gate and runs ONE mod per arm.
# This runs both at once, which is the only way to exercise the thing a single
# arm cannot reach: --enable-lua-udp binds ONE socket for the whole game, so
# every inbound datagram raises on_udp_packet_received in EVERY mod. Two mods in
# one game is one wire both of them hear, and what keeps their sessions apart is
# the source-port filter in guest/{go,rust}/fkipc. Run one mod and that
# machinery is never touched; run two and it is touched on every frame.
#
#   FACTORIO_USERDIR=/tmp/fkuser ./scripts/run-ipcdemo.sh                # headless MP, automated
#   FACTORIO_USERDIR=/tmp/fkuser ./scripts/run-ipcdemo.sh --smoke        # ...the same thing
#   FACTORIO_USERDIR=/tmp/fkuser ./scripts/run-ipcdemo.sh --smoke-single # GRAPHICAL SINGLE PLAYER, automated
#   FACTORIO_USERDIR=/tmp/fkuser ./scripts/run-ipcdemo.sh --play         # set up a live MP session
#   FACTORIO_USERDIR=/tmp/fkuser ./scripts/run-ipcdemo.sh --play --single # ...a live SINGLE-PLAYER one
#   ... --play --launch                                                  # ...and start the client too
#
# --smoke is the gate: headless server, scripted slider sequence to BOTH mods,
# named PASS/FAIL legs, no display anywhere. --play builds the same thing and
# hands it to a person: a save to load, a page to open, and the exact command
# line to start the graphical game with.
#
# TWO ENVIRONMENTS, AND THE SECOND EXISTS BECAUSE A DEFECT SHIPPED THROUGH THE
# FIRST. Everything above runs a HEADLESS SERVER. --single runs ONE GRAPHICAL
# SINGLE-PLAYER PROCESS instead -- no server, no client, no join -- which is the
# environment fkipc's ProfileClient is named after and which nothing in this
# repo covered until 2026-08-07. ProfileClient's whole point is to omit
# for_player, because a client has no server; that normalisation was applied to
# a copy of the config AFTER the transport had been built from the raw one, so
# every frame went out with for_player = 0. That is "the server if present" --
# exactly right on the one topology both headless gates run, and a SILENT no-op
# in single player. Two green gates and an entirely dead arm. --smoke-single is
# the gate that would have caught it; see agents/ipc.md, "The graphical
# single-player re-run".
#
# A SINGLE-PLAYER RUN NEEDS ONE PIECE OF SCAFFOLDING AND IT IS NOT ABOUT IPC. A
# --create'd freeplay map opens a MODAL at tick 750 in single player
# (base/script/freeplay: show_intro_message takes the game.show_message_dialog
# branch when the game is not multiplayer), and a modal PAUSES single player
# until somebody clicks it. So the single-player arms build a bare Lua mod,
# fk-demo-nointro, into their own mod directory to remove it -- and that mod is
# also the run's CLOCK WITNESS, logging game.tick with no wasm anywhere in it. A
# headless server takes the player.print branch and has no modal to open, which
# is why every other in-game gate here is blind to this.
#
# ALT-TAB FREELY IN EITHER ENVIRONMENT. Graphical single player ticks at 60/s
# defocused, and at 60/s in a window that was never focused from the moment it
# existed -- measured 2026-08-07 on 2.1.14 with the window state read from
# `lsappinfo front`. Focus is not a variable and audio is not a variable; the
# modal above is what "the game stops when I look away" turned out to be.
#
# Two mods is also the only way to exercise the OTHER filter the source port
# cannot cover: the two demo mods carry build-time IDENTITY TOKENS
# ("fk-demo-daylight/1", "fk-demo-circle/1"), and --smoke's identity leg crosses
# them, which is a handshake correct at every transport layer between two ends
# that disagree about what the channels mean. That leg runs LAST and re-dials
# both companions, because one socket per mod port means a mismatched session
# cannot be held beside a matched one -- which is why each guest logs exactly
# TWO session-up lines over a run rather than one.
#
# THE ENGINE FLOOR IS REAL AND THIS SCRIPT CANNOT WORK AROUND IT. On 2.0.77 a
# headless recv_udp with a packet queued kills the server at TickClosure.cpp:91,
# a C++ abort no pcall can catch. Below MinEngineVersion = 2.1.14 both guest
# libraries are HARD-DISABLED: no HELLO, no pump, not one datagram. So this
# script REFUSES TO START on an older install rather than running: every leg
# would sit at its deadline and report a protocol failure for an engine that is
# simply too old. The floor is read out of the library, not written here; see
# scripts/lib-engine.sh.
#
# THE FLOOR IS ABOUT THE ENGINE AND NOT ABOUT THE API PIN, which is a separate
# axis: the pin decides which description the bindings came from and defaults to
# the general-availability release, while fkipc asks helpers.game_version at run
# time. Both demo mods here are packaged at the default pin and declare the
# INSTALLED engine's series in info.json, which is the only combination that
# both builds and loads on this machine.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
WORK="$ROOT/testdata/tmp"
OUT="$WORK/ipcdemo"
# A PRIVATE USERDIR BY DEFAULT, which follows run-ipc.sh and run-ipcprobe.sh
# rather than run-guest.sh, and for their reason: this holds a socket open for a
# minute or an hour, and Factorio LOCKS its user directory, so defaulting to the
# installed one makes "somebody had the game open" the most likely outcome of a
# first run.
USERDIR="${FACTORIO_USERDIR:-$WORK/ipcdemo-userdir}"

GAME_PORT="${GAME_PORT:-29433}"      # --enable-lua-udp: the game's ONE socket
# The two companion ports. NEITHER IS A FREE CHOICE: they are compiled into
# guest/go/examples/demo-daylight and guest/rust/examples/demo-circle as
# Config.Port, because a guest has no configuration file. Overriding one here
# without editing that guest gives a companion listening to nobody.
DAYLIGHT_PORT="${DAYLIGHT_PORT:-29434}"
CIRCLE_PORT="${CIRCLE_PORT:-29437}"
HTTP_ADDR="${HTTP_ADDR:-:8080}"
SERVER_PORT="${SERVER_PORT:-34197}"  # the game's MULTIPLAYER port (--play's client connects here)
SMOKE_TIMEOUT="${SMOKE_TIMEOUT:-150}"

MODE=smoke
LAUNCH=0
SINGLE=0
for arg in "$@"; do
  case "$arg" in
    --smoke) MODE=smoke ;;
    --play)  MODE=play ;;
    --single) SINGLE=1 ;;
    --smoke-single) MODE=smoke; SINGLE=1 ;;   # sugar for --smoke --single
    --launch) LAUNCH=1 ;;
    -h|--help) sed -n '2,68p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $arg (want --smoke, --smoke-single, --play, --single or --launch)" >&2; exit 1 ;;
  esac
done
# --launch is the MULTIPLAYER arm's "start the client for me". A single-player
# run has no client to start -- the one process IS the game -- so it always
# launches, and saying so is better than silently ignoring a flag.
if [ "$SINGLE" = 1 ] && [ "$LAUNCH" = 1 ]; then
  echo "note: --launch is implied by --single (there is only one process to start)" >&2
fi

DAYLIGHT_MOD=fk-demo-daylight
CIRCLE_MOD=fk-demo-circle
# TEST SCAFFOLDING, built by this script into the single-player mod directory
# and never committed as a mod -- the run-roundtrip.sh fk-savetrigger precedent.
# It contains no wasm and nothing about IPC: it removes the freeplay intro modal
# and logs the game clock. See build_nointro.
NOINTRO_MOD=fk-demo-nointro
MODDIR="$WORK/ipcdemo-mods"
# The single-player arms get their OWN mod directory and their OWN map, rather
# than nointro being added to the shared ones. Two reasons, and the second is
# the load-bearing one: a save records its mod set, so a map created without
# nointro and loaded with it is a mod-change prompt in a graphical game with
# nobody to click it; and the headless gate that has run for months should not
# quietly change environment because a new arm was added beside it. The two
# fkipc mods are the SAME BYTES in both directories -- copied, not repackaged.
SMODDIR="$WORK/ipcdemo-mods-single"
MAP="$USERDIR/saves/fkipc-demo.zip"
SMAP="$USERDIR/saves/fkipc-demo-single.zip"
DEMOBIN="$WORK/ipcdemo-bin"
DAYLIGHT_WASM="$WORK/ipcdemo-daylight.wasm"
CIRCLE_WASM="$WORK/ipcdemo-circle.wasm"

# The freeplay intro modal's tick, and the margin the clock leg wants past it.
# 750 is measured (agents/ipc.md): the crash-site cutscene ends there and
# show_intro_message opens a dialog that stops single player dead.
MODAL_TICK=750
CLOCK_FLOOR=$((MODAL_TICK + 150))

# Every path here is interpolated into a Factorio command line and some into
# JSON. A newline in one is how sharding stage C created three directories NAMED
# BY SHELL OUTPUT, which rode into a commit with nothing failing. Refuse rather
# than quote harder.
check_path() {
  case "$2" in
    "") echo "internal error: $1 is empty" >&2; exit 1 ;;
    *"
"*) echo "internal error: $1 contains a NEWLINE. Value was:" >&2
        printf '  %q\n' "$2" >&2; exit 1 ;;
  esac
}
check_path ROOT "$ROOT"
check_path WORK "$WORK"
check_path FACTORIO_BIN "$FACTORIO"
check_path USERDIR "$USERDIR"
check_path MODDIR "$MODDIR"
check_path SMODDIR "$SMODDIR"
check_path MAP "$MAP"
check_path SMAP "$SMAP"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"
for p in "$DAYLIGHT_PORT" "$CIRCLE_PORT"; do
  [ "$p" != "$GAME_PORT" ] || {
    echo "a companion port equals GAME_PORT ($GAME_PORT): --enable-lua-udp binds one" >&2
    echo "  socket and that port is also the SOURCE port of everything the game sends." >&2
    exit 1; }
done
[ "$DAYLIGHT_PORT" != "$CIRCLE_PORT" ] || {
  echo "DAYLIGHT_PORT and CIRCLE_PORT must differ: the DESTINATION port is the only" >&2
  echo "  thing routing a frame to the right companion." >&2; exit 1; }

# THE BACKGROUNDED FACTORIO AND COMPANION DIE WITH THE SCRIPT THAT STARTED THEM.
# An orphaned server holds the user directory lock, which is the state the
# operations notes describe as "dies at startup and reads as a broken gate" for
# every later in-game run, until somebody finds the process by hand.
#
# The EXIT trap does NOT exit and the signal traps MUST -- an exiting EXIT trap
# replaces this script's own status, and a TERM trap that does not exit
# SWALLOWS the signal and lets the script carry on against a game it has just
# killed. AND THE KILL ESCALATES, because a Factorio has taken minutes to act on
# a plain SIGTERM here and nothing downstream can tell that from a hung harness.
GAME_PID=""
DEMO_PID=""
CLIENT_PID=""
stop_game() {
  [ -n "$GAME_PID" ] || return 0
  kill "$GAME_PID" 2>/dev/null || true
  local i=0
  while kill -0 "$GAME_PID" 2>/dev/null && [ $i -lt 20 ]; do sleep 0.25; i=$((i + 1)); done
  kill -9 "$GAME_PID" 2>/dev/null || true
  wait "$GAME_PID" 2>/dev/null || true
  GAME_PID=""
}
stop_all() {
  [ -n "$GAME_PID" ] && kill -9 "$GAME_PID" 2>/dev/null || true
  [ -n "$CLIENT_PID" ] && kill -9 "$CLIENT_PID" 2>/dev/null || true
  [ -n "$DEMO_PID" ] && kill "$DEMO_PID" 2>/dev/null || true
  return 0
}
trap stop_all EXIT
trap 'stop_all; exit 130' INT
trap 'stop_all; exit 143' TERM

mkdir -p "$WORK" "$OUT" "$USERDIR" "$USERDIR/saves"

# Factorio LOCKS its user directory, so a run started while a game is open dies
# at startup. Pointing FACTORIO_USERDIR somewhere is not enough on its own --
# that only tells this script where to READ logs -- so the config.ini the GAME
# is handed has to say the same write-data. Copied from the installed one rather
# than written from scratch, because read-data is not guessable from the
# executable's path on every platform.
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

# All three ports free before anything binds them. A companion whose port is
# already taken fails in a way that looks exactly like "the game never replied",
# which is the finding this script exists to establish.
python3 - "$GAME_PORT" "$DAYLIGHT_PORT" "$CIRCLE_PORT" <<'PY' || exit 1
import socket, sys
for p in sys.argv[1:]:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.bind(("127.0.0.1", int(p)))
    except OSError as e:
        print(f"UDP port {p} on 127.0.0.1 is not free: {e}", file=sys.stderr)
        print("  Set GAME_PORT / DAYLIGHT_PORT / CIRCLE_PORT, or stop whatever holds it.",
              file=sys.stderr)
        sys.exit(1)
    finally:
        s.close()
PY

# auto_pause IS THE LOAD-BEARING KEY, and it is the probe's finding rather than
# a copied default. A headless server with nobody connected PAUSES: the update
# loop stops, on_tick never fires, the pump never runs and recv_udp is never
# called -- which reads exactly like "UDP does not work on this build".
SETTINGS="$WORK/ipcdemo-server-settings.json"
cat > "$SETTINGS" <<'JSON'
{ "name": "fkipc-demo", "description": "", "visibility": { "public": false, "lan": false },
  "username": "", "password": "", "token": "",
  "require_user_verification": false, "max_upload_in_kilobytes_per_second": 0,
  "minimum_latency_in_ticks": 0, "ignore_player_limit_for_returning_players": false,
  "allow_commands": "true", "autosave_interval": 1000000, "autosave_slots": 5,
  "afk_autokick_interval": 0, "auto_pause": false, "only_admins_can_pause_the_game": true,
  "autosave_only_on_server": true, "non_blocking_saving": false }
JSON

echo "==> Factorio $("$FACTORIO" --version 2>/dev/null | head -1 | sed 's/Version: //')"
# EARLY, BEFORE ANY MAP IS MADE OR ANY GAME STARTED. Below the floor no session
# can come up, and an interactive --play run would leave a person clicking at a
# window for a minute before finding that out.
require_fkipc_floor
if [ "$SINGLE" = 1 ]; then
  echo "==> mode $MODE --single (ONE GRAPHICAL single-player process; a window will open)"
else
  echo "==> mode $MODE (headless server)"
fi
echo "==> game udp $GAME_PORT, daylight companion $DAYLIGHT_PORT, circle companion $CIRCLE_PORT"
echo "==> userdir $USERDIR"

# ---------------------------------------------------------------------------
# Build. BOTH WASM FILES ARE ALWAYS REBUILT, for run-roundtrip.sh's recorded
# reason: a cached guest cost sharding stage C four wrong conclusions in a row,
# each fix re-run against the binary the first invocation had built.
# ---------------------------------------------------------------------------
command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
command -v wasm-opt >/dev/null || { echo "wasm-opt is not installed: brew install binaryen" >&2; exit 1; }
command -v cargo >/dev/null || { echo "cargo is not installed: https://rustup.rs" >&2; exit 1; }

echo "==> building guest/go/examples/demo-daylight"
rm -f "$DAYLIGHT_WASM"
( cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
    -gc=leaking -opt=2 -o "$DAYLIGHT_WASM" ./examples/demo-daylight )
[ -s "$DAYLIGHT_WASM" ] || { echo "tinygo produced no $DAYLIGHT_WASM" >&2; exit 1; }

echo "==> building guest/rust/examples/demo-circle"
rm -f "$CIRCLE_WASM"
( cd "$ROOT/guest/rust" && CARGO_TARGET_DIR="$WORK/cargo-ipcdemo" \
    cargo build --release --target wasm32-unknown-unknown -p demo-circle )
cp "$WORK/cargo-ipcdemo/wasm32-unknown-unknown/release/demo_circle.wasm" "$CIRCLE_WASM"
[ -s "$CIRCLE_WASM" ] || { echo "cargo produced no $CIRCLE_WASM" >&2; exit 1; }

echo "==> packaging both mods into one mod directory"
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"
rm -rf "$MODDIR"; mkdir -p "$MODDIR"
# --opt UNSET means "whatever fklua defaults to", which is the point: a pinned
# level here is how a script quietly stops testing the default.
"$ROOT/bin/fklua" mod "$DAYLIGHT_WASM" --name "$DAYLIGHT_MOD" --version 0.1.0 \
  --author FkLua --description "fkipc demo: drag the sun (Go)" \
  --factorio-version "$SERIES" -o "$MODDIR" | sed 's/^/    /'
"$ROOT/bin/fklua" mod "$CIRCLE_WASM" --name "$CIRCLE_MOD" --version 0.1.0 \
  --author FkLua --description "fkipc demo: resize a circle (Rust)" \
  --factorio-version "$SERIES" -o "$MODDIR" | sed 's/^/    /'
cat > "$MODDIR/mod-list.json" <<JSON
{"mods":[{"name":"base","enabled":true},
         {"name":"$DAYLIGHT_MOD","enabled":true},
         {"name":"$CIRCLE_MOD","enabled":true}]}
JSON

# fk-demo-nointro: TEST SCAFFOLDING for the single-player arms, written here
# rather than committed as a mod -- run-roundtrip.sh builds fk-savetrigger the
# same way, for the same reason: it is part of the harness, not of FkLua.
#
# It contains no wasm, no FkLua runtime and nothing about IPC. It does two
# things, and the second is worth as much as the first.
build_nointro() {
  local dir="$1"
  rm -rf "$dir/${NOINTRO_MOD}_0.1.0"; mkdir -p "$dir/${NOINTRO_MOD}_0.1.0"
  cat > "$dir/${NOINTRO_MOD}_0.1.0/info.json" <<JSON
{"name":"$NOINTRO_MOD","version":"0.1.0","title":"FkLua demo: no intro modal",
 "author":"FkLua","factorio_version":"$SERIES","dependencies":["base"]}
JSON
  cat > "$dir/${NOINTRO_MOD}_0.1.0/control.lua" <<'LUA'
-- Test scaffolding for scripts/run-ipcdemo.sh's single-player arms. Not part of
-- FkLua: no wasm, no runtime, no IPC. It takes the base game's opening sequence
-- out of the way, and it is the run's independent witness that the clock moves.
--
-- WHY IT EXISTS. A --create'd freeplay map opens a MODAL DIALOG at tick 750 in
-- SINGLE PLAYER, and a modal pauses single player until a person clicks it:
--
--   base/script/freeplay/freeplay.lua, show_intro_message:
--     if game.is_multiplayer() then player.print(...)
--     else game.show_message_dialog{text = ...} end
--
-- reached from on_cutscene_waypoint_reached when the crash-site intro ends.
-- Measured 2026-08-07 on 2.1.14: 60 ticks/s up to tick 750, then 0.00 ticks/s
-- INDEFINITELY, with the window focused and frontmost -- which is how "single
-- player stops ticking when it loses focus" came to be believed here. A
-- headless server takes the player.print branch and has no dialog to open, so
-- every headless gate in this repo is blind to it. Confirmed again here as this
-- mod's own negative control: with the two remote calls below disabled, the
-- game's last logged tick is 750 and stays 750 for as long as it is left alone.
--
-- THREE REMOVALS, AND WHERE EACH ONE HAPPENS IS THE WHOLE DESIGN.
--
--   1. set_disable_crashsite(true), FROM on_init. freeplay creates the crash
--      site and starts the cutscene inside on_player_created, so anything done
--      on the first TICK is already too late -- the player exists by then and
--      the flyover has begun. on_init runs at --create, before any player
--      exists at all, and what it writes lands in freeplay's own storage and is
--      SAVED IN THE MAP. That is why the single-player map is recreated on
--      every run: this mod's behaviour is baked into it.
--   2. set_skip_intro(true). show_intro_message's first line is
--      `if storage.skip_intro then return end`, and this is the MEASURED fix
--      (agents/ipc.md, "The graphical single-player re-run"). With (1) in place
--      there is no cutscene to end, so this covers the OTHER call site: the one
--      on_player_created makes directly once the crash site is disabled.
--   3. leave the crash-site cutscene, from the first tick, as the fallback for
--      a map that was NOT created with this mod -- on_init does not run for
--      one, so (1) never happened and the flyover is already underway. The
--      intro message is shown from on_cutscene_waypoint_reached and NOT from
--      on_cutscene_cancelled, so leaving early is a second path away from the
--      same dialog. There is no remote call for it: freeplay binds
--      skip_crash_site_cutscene to a CUSTOM INPUT, which a script cannot raise.
--      player.exit_cutscene() is the API that does, and it is a BOUND CLOSURE --
--      handing the player in a second time is an argument-count error on every
--      method in this API.
--
-- Both remote calls are pcall'd and both are repeated on the first tick: a
-- non-freeplay map has no such interface and this mod must be a no-op there
-- rather than an error, and the retry costs nothing if on_init got there first.
-- Nothing else in freeplay opens a dialog, so that is the whole of it.
--
-- AND IT IS THE CLOCK WITNESS. The heartbeat below reads game.tick with no
-- wasm, no guest heap and no UDP anywhere in it, which is what makes it
-- evidence about the GAME rather than about fkipc's reading of the game: if the
-- dialog ever comes back, the last tick in the log is 750 and every fkipc leg
-- fails at once for a reason that is not fkipc's.

local INTRO_UNTIL = 900   -- comfortably past the tick-750 dialog, then stop
local HEARTBEAT   = 300   -- five seconds of game time

-- Returns true only if BOTH calls landed: a partial success is a map that still
-- has something to say, and the log line should not claim otherwise.
local function quiet_freeplay()
  local a = pcall(remote.call, "freeplay", "set_disable_crashsite", true)
  local b = pcall(remote.call, "freeplay", "set_skip_intro", true)
  return a and b
end

script.on_init(function()
  log("[fk-nointro] freeplay " ..
      (quiet_freeplay() and "quietened at on_init -- no crash site, no cutscene, no dialog"
                        or "unavailable at on_init -- not a freeplay map"))
end)

local function intro(e)
  if not storage.fk_nointro_ran then
    storage.fk_nointro_ran = true
    log("[fk-nointro] freeplay " ..
        (quiet_freeplay() and "quietened at first tick" or
         "unavailable at first tick -- not a freeplay map, nothing to skip"))
  end
  if not storage.fk_nointro_cut then
    for _, p in pairs(game.connected_players) do
      if p.controller_type == defines.controllers.cutscene and pcall(p.exit_cutscene) then
        storage.fk_nointro_cut = true
        log("[fk-nointro] left the crash-site cutscene at tick " .. e.tick)
      end
    end
  end
  -- A player created later still finds skip_intro set: it lives in freeplay's
  -- own storage and survives this handler going away.
  if e.tick >= INTRO_UNTIL then script.on_event(defines.events.on_tick, nil) end
end

script.on_event(defines.events.on_tick, intro)
script.on_nth_tick(HEARTBEAT, function(e) log("[fk-nointro] tick=" .. e.tick) end)
LUA
}

if [ "$SINGLE" = 1 ]; then
  echo "==> building the single-player mod directory (the two mods plus $NOINTRO_MOD)"
  rm -rf "$SMODDIR"; mkdir -p "$SMODDIR"
  # COPIED rather than repackaged, so the single-player arm runs the SAME BYTES
  # the headless arm does -- a difference between the two environments should
  # never be able to come from a second `fklua mod` invocation.
  cp -R "$MODDIR/${DAYLIGHT_MOD}_0.1.0" "$MODDIR/${CIRCLE_MOD}_0.1.0" "$SMODDIR/"
  build_nointro "$SMODDIR"
  cat > "$SMODDIR/mod-list.json" <<JSON
{"mods":[{"name":"base","enabled":true},
         {"name":"$DAYLIGHT_MOD","enabled":true},
         {"name":"$CIRCLE_MOD","enabled":true},
         {"name":"$NOINTRO_MOD","enabled":true}]}
JSON
fi

# THE COMPANION IS BUILT FROM THE SDK MODULE, as an outside tool would build it.
# That is a gate in itself: sdk/go is a separate module whose only dependency on
# this repo is guest/go/fkipc/wire, so a //go:wasmimport leaking into that
# package fails here rather than in somebody's build.
echo "==> building the companion from sdk/go"
( cd "$ROOT/sdk/go" && go build -o "$DEMOBIN" ./cmd/ipcdemo )

# ---------------------------------------------------------------------------
# The map, created WITH every mod that will later load it, so a mod that fails
# to load fails here rather than inside a measured window -- and so that no
# graphical run ever meets a mod-change prompt with nobody to click it.
# ---------------------------------------------------------------------------
# THE STALE-MAP TRAP, MADE MECHANICAL. It has now cost three runs.
#
# `fklua mod` stamps each packaged module with a BUILD ID over the guest wasm and
# the --api pin, and `same_build()` is what decides whether a load ADOPTS the
# saved guest heap or rebuilds a fresh one. A cached map carries the stamp of the
# mods that created it, so any change to the wasm, to the pin, or to the stamp's
# own definition makes that map a map of a different program -- and the failure
# is silent and asymmetric: the server declines to adopt and runs on from tick 0,
# a joining client declines too and rebuilds a tick-0 heap against a world that
# is already at tick 1250, and the two CRC differently from the first joined
# tick. Nothing warns, because on_configuration_changed fires on a VERSION
# change and every dev rebuild keeps the version.
#
# So the map is checked against the mods rather than trusted, and FRESH=1 stops
# being something anybody has to remember. A substring search is enough: the
# stamp is a 16-hex literal in fk_module.lua and Factorio writes storage as
# strings into script.dat, so if the map was made by these mods every stamp is
# in there verbatim.
map_is_current() {   # map_is_current <map> <moddir> <mod>...
  local map="$1" moddir="$2"; shift 2
  [ -f "$map" ] || return 1
  python3 - "$map" "$moddir" "$@" <<'PY'
import re, sys, zipfile
mapfile, moddir, mods = sys.argv[1], sys.argv[2], sys.argv[3:]
want = {}
for m in mods:
    try:
        src = open(f"{moddir}/{m}_0.1.0/fk_module.lua", "rb").read()
    except OSError:
        continue          # a mod with no fk_module.lua is bare Lua: nothing to stamp
    hit = re.search(rb'\bbuild\s*=\s*"([0-9a-f]{8,})"', src)
    if hit:
        want[m] = hit.group(1)
if not want:
    sys.exit(0)           # nothing stamped -- nothing this check can say
try:
    with zipfile.ZipFile(mapfile) as z:
        blob = b"".join(z.read(n) for n in z.namelist() if n.endswith("script.dat"))
except Exception as e:
    print(f"unreadable: {e}")
    sys.exit(1)
missing = [m for m, s in want.items() if s not in blob]
if missing:
    print(" ".join(missing))
    sys.exit(1)
PY
}

create_map() {   # create_map <map> <moddir> <logfile> <mod>...
  local map="$1" moddir="$2" log="$3"; shift 3
  if [ -f "$map" ] && [ -z "${FRESH:-}" ] && ! map_is_current "$map" "$moddir" "$@" >/dev/null; then
    echo "==> the map was built by another build of these mods; recreating it"
    rm -f "$map"
  fi
  if [ ! -f "$map" ] || [ -n "${FRESH:-}" ]; then
    echo "==> creating $(basename "$map")"
    rm -f "$map"
    "$FACTORIO" "${CFGARG[@]}" --mod-directory "$moddir" --create "$map" \
      --map-gen-seed 1 --disable-audio >"$log" 2>&1 \
      || { echo "map creation failed; see $log" >&2
           tail -30 "$log" >&2; exit 1; }
  fi
  local m
  for m in "$@"; do
    grep -q "Checksum for script __${m}__" "$log" \
      || { echo "$m did not load during --create: every run below would be missing it." >&2
           echo "  See $log" >&2; exit 1; }
  done
}
if [ "$SINGLE" = 1 ]; then
  # ALWAYS RECREATED, and it is not tidiness. fk-demo-nointro disables the crash
  # site from on_init, which runs HERE and writes into freeplay's storage, so
  # this mod's behaviour is part of the saved map rather than of the run. A
  # cached map is then a run measuring a scaffolding mod that no longer exists --
  # which is the stale-input failure run-roundtrip.sh's cached wasm cost sharding
  # stage C four wrong conclusions over. Two seconds.
  rm -f "$SMAP"
  create_map "$SMAP" "$SMODDIR" "$OUT/create-single.log" \
    "$DAYLIGHT_MOD" "$CIRCLE_MOD" "$NOINTRO_MOD"
else
  create_map "$MAP" "$MODDIR" "$OUT/create.log" "$DAYLIGHT_MOD" "$CIRCLE_MOD"
fi

FAILED=0
say_pass() { printf 'PASS %s -- %s\n' "$1" "$2"; }
say_fail() { printf 'FAIL %s -- %s\n' "$1" "$2"; FAILED=1; }
server_alive() { [ -n "$GAME_PID" ] && kill -0 "$GAME_PID" 2>/dev/null; }

# One mod's own session lines, out of the captured server log. The log line
# carries the mod that wrote it -- `@__fk-demo-circle__/control.lua` -- which is
# the only thing distinguishing two mods that log the same words.
guest_lines() { sed -n "s|.*__${2}__.*\(fkipc session .*\)$|\1|p" "$1"; }

# Every tick fk-demo-nointro logged, in order. Bare Lua reading game.tick: no
# wasm, no guest heap, no UDP, so it is evidence about the GAME's clock and not
# about fkipc's reading of it.
nointro_ticks() { sed -n 's|.*\[fk-nointro\] tick=\([0-9][0-9]*\).*|\1|p' "$1"; }

# ---------------------------------------------------------------------------
# Starting ONE GRAPHICAL Factorio straight into a save. Shared by --smoke-single
# and --play --single, and it is where the two Steam-shaped traps live.
# ---------------------------------------------------------------------------
start_graphical_game() {   # start_graphical_game <moddir> <map> <log> [extra args...]
  local moddir="$1" map="$2" log="$3"; shift 3
  # THE STEAM RELAUNCH TRAP. Launching the Steam build's binary directly makes
  # Steamworks relaunch the game THROUGH Steam, dropping every command-line
  # argument -- so the game would come up with the installed user directory and
  # no mods, i.e. a run that looks like the mods never loaded. Steamworks skips
  # the relaunch when it already knows the appid: steam_appid.txt in the
  # process's WORKING DIRECTORY plus SteamAppId and SteamGameId in its
  # environment. 427520 is Factorio.
  printf '427520\n' > "$USERDIR/steam_appid.txt"
  rm -f "$log"
  # `exec` IS LOAD-BEARING and is the difference between a teardown that works
  # and one that reports success. Without it $! is the SUBSHELL's pid, so every
  # kill in stop_game reaches a shell that has already forked the game, leaving
  # an orphaned Factorio holding this user directory's lock -- which the
  # operations notes describe as "dies at startup and reads as a broken gate"
  # for every later in-game run, until somebody finds the process by hand.
  ( cd "$USERDIR" || exit 1
    exec env SteamAppId=427520 SteamGameId=427520 \
      "$FACTORIO" "${CFGARG[@]}" --mod-directory "$moddir" \
      --load-game "$map" --enable-lua-udp "$GAME_PORT" "$@"
  ) > "$log" 2>&1 &
  GAME_PID=$!
}

# A graphical game is READY when it has opened the lua-udp socket, which it does
# after the map and its scripts are loaded. On this machine that is ~17 s with a
# warm sprite cache and appreciably longer with a cold one -- everything before
# `Factorio initialised` is atlas work and has nothing to do with what is being
# tested here -- so the budget is deliberately generous rather than tuned.
wait_graphical_ready() {   # wait_graphical_ready <log>
  local log="$1" i=0
  until grep -q "Opening socket" "$log" 2>/dev/null; do
    server_alive || { echo "    the game exited during startup; see $log" >&2
                      tail -20 "$log" >&2; return 1; }
    if [ $i -ge 600 ]; then
      echo "    the game never opened the lua-udp socket in 300 s; see $log" >&2
      return 1
    fi
    [ $((i % 40)) -ne 0 ] || [ $i -eq 0 ] || echo "    ...still loading ($((i / 2)) s)"
    sleep 0.5; i=$((i + 1))
  done
  return 0
}

# AND THEN IT HAS TO GET PAST THE DIALOG, WHICH IS NOT THE SAME THING.
#
# This is the single-player arm's whole reason for existing and it is easy to
# build without: the scripted conversation is over in about seven seconds of
# GAME time, so a run that starts talking as soon as the socket opens finishes
# four hundred ticks short of tick 750 and never meets the modal at all. The
# first build of this gate did exactly that and reported the fkipc legs green
# while proving nothing about the environment.
#
# Waiting FIRST rather than checking afterwards is what makes every leg below a
# measurement taken on the FAR SIDE of the dialog. If the dialog ever comes
# back, this is what fails, and it fails naming it -- rather than nine fkipc
# legs failing at once because the guest stopped being pumped.
wait_past_modal() {   # wait_past_modal <log>
  local log="$1" i=0 last=0
  while [ $i -lt 240 ]; do
    last="$(nointro_ticks "$log" | tail -1)"
    [ -n "$last" ] || last=0
    [ "$last" -lt "$CLOCK_FLOOR" ] || { CLOCK_AT_START="$last"; return 0; }
    server_alive || { echo "    the game exited before tick $CLOCK_FLOOR; see $log" >&2
                      return 1; }
    sleep 0.25; i=$((i + 1))
  done
  echo "    the game's own clock stopped at tick $last, short of $CLOCK_FLOOR." >&2
  return 1
}

# ---------------------------------------------------------------------------
if [ "$MODE" = play ] && [ "$SINGLE" = 1 ]; then
  # THE SIMPLE TOPOLOGY: one process, which is the game AND the simulation.
  #
  # It is the environment fkipc's ProfileClient is for, and it is a real option
  # rather than a fallback -- graphical single player ticks at 60/s alt-tabbed
  # and at 60/s in a window that was never focused (see the header). What the
  # multiplayer arm below buys instead is immunity to anything a client-side GUI
  # does, and a JOIN, which is the only place a peer-local guest write shows up.
  # Neither of those is what a person dragging a slider needs.
  SRUNMAP="$WORK/ipcdemo-play-single-map.zip"
  check_path "the run map" "$SRUNMAP"
  rm -f "$SRUNMAP"; cp "$SMAP" "$SRUNMAP"   # keep $SMAP pristine across sessions

  echo
  echo "==> starting the companion (GUI mode)"
  "$DEMOBIN" -game-port "$GAME_PORT" -daylight-port "$DAYLIGHT_PORT" \
    -circle-port "$CIRCLE_PORT" -http "$HTTP_ADDR" &
  DEMO_PID=$!
  sleep 0.5
  kill -0 "$DEMO_PID" 2>/dev/null || { echo "the companion exited immediately" >&2; exit 1; }

  echo "==> starting your game (a window will open; ~20 s of sprite loading first)"
  # Audio left ON here, unlike the gate: this arm is for a person. It is not a
  # variable either way -- the focus table in the header has both rows.
  start_graphical_game "$SMODDIR" "$SRUNMAP" "$OUT/play-single-game.log"
  wait_graphical_ready "$OUT/play-single-game.log" \
    || { echo "the game never reached the map; see $OUT/play-single-game.log" >&2; exit 1; }
  # Said out loud rather than left to be discovered at tick 750, twelve seconds
  # into somebody's demo, as a dialog they have to click.
  grep -q "\[fk-nointro\] freeplay quietened" "$OUT/create-single.log" \
    || echo "    NOTE: $NOINTRO_MOD could not reach freeplay's remote interface when this" \
            "map was created, so it may open an intro dialog at tick $MODAL_TICK." \
            "Click it and carry on." >&2

  URL="http://localhost:${HTTP_ADDR#*:}"
  cat <<EOF

  ---------------------------------------------------------------------------
  READY. Your game is running the world itself -- there is no server here and
  nothing else to start.

  OPEN $URL and drag the sliders.

  What to look for: the Daytime slider drags the sun across the sky and it
  STAYS where you put it (the day/night cycle is frozen). The Radius and Hue
  sliders resize and recolour a circle drawn at spawn, and the "entities
  inside" readout counts what is actually in it -- walk into the circle and
  the count goes up by one. Walk anywhere and the player position readout on
  the left-hand card follows you.

  ALT-TAB FREELY. Single player ticks at 60/s with the window in the
  background, and at 60/s in a window that was never focused from the moment
  it existed -- measured on 2.1.14, window state read from \`lsappinfo front\`.
  What DOES stop single player is a modal dialog, and a freshly created
  freeplay map opens exactly one at tick 750; $NOINTRO_MOD is in this run's
  mod directory to take it away, which is also why you land on the map instead
  of in the crash-site cutscene.

  Both mods talk through this game's one socket ($GAME_PORT); each refuses the
  other's datagrams on the source port -- the machinery --smoke's foreign-port
  leg proves.

  Ctrl-C here when you are done -- it stops the game and the companion.
  Game log: $OUT/play-single-game.log
  ---------------------------------------------------------------------------

EOF
  wait "$DEMO_PID" 2>/dev/null || true
  exit 0
fi

# ---------------------------------------------------------------------------
if [ "$MODE" = play ]; then
  # THE SIMULATION RUNS ON A HEADLESS SERVER, AND THE PLAYER'S GAME IS A VIEW.
  #
  # THIS COMMENT USED TO SAY "SINGLE PLAYER STOPS TICKING WHEN THE GAME WINDOW
  # LOSES FOCUS". IT IS FALSE, and the measurement behind it was confounded two
  # ways at once: every dead run was a background-script launch of a --create'd
  # freeplay map running fkipc mods, and BOTH of those were separately fatal
  # for reasons that have nothing to do with focus. Re-measured 2026-08-07 on
  # 2.1.14 with a bare-Lua mod logging its own tick, 45 s a phase, the window
  # state read from `lsappinfo front`:
  #
  #   audio on,  focused / defocused / refocused        59.87 / 60.00 / 59.87
  #   audio off, focused / defocused / refocused        59.87 / 59.87 / 59.87
  #   audio on,  NEVER focused / activated / defocused  59.87 / 61.30 / 59.87
  #   audio off, NEVER focused / activated / defocused  60.00 / 59.87 / 59.87
  #
  # Graphical single player ticks at 60/s alt-tabbed, and at 60/s in a window
  # that was never focused from the moment it existed. Focus is not a variable,
  # audio is not a variable, and NSAppSleepDisabled is not a lever because App
  # Nap is not what was happening.
  #
  # WHAT ACTUALLY KILLED THOSE RUNS WAS TWO THINGS, and both are worth knowing:
  #
  #   1. A --create'd FREEPLAY MAP OPENS A MODAL DIALOG AT TICK 750, and a
  #      modal pauses single player until a person clicks it. base/script/
  #      freeplay/freeplay.lua: the crash-site cutscene ends, and
  #      show_intro_message calls game.show_message_dialog in SINGLE PLAYER
  #      where it calls player.print in multiplayer. Measured: 60 ticks/s up to
  #      tick 750, then 0.00 ticks/s indefinitely -- WITH THE WINDOW FOCUSED,
  #      which is exactly how "unfocused" came to be blamed for it. It is also
  #      why the headless topology looked like the cure: a server takes the
  #      player.print branch and has no modal to open.
  #      `remote.call("freeplay", "set_skip_intro", true)` before tick 750
  #      removes it, and the same map then runs indefinitely in every focus
  #      state, which is the table above.
  #
  #   2. fkipc COULD NOT HOLD A SESSION IN SINGLE PLAYER AT ALL, for a reason
  #      with nothing to do with ticking. Open() built the transport from the
  #      raw Config, and ProfileClient's whole point -- omit for_player,
  #      because a client has no server -- is applied in configure(), one line
  #      later and to a different copy. So the send path kept for_player = 0,
  #      which is "the server if present" and a SILENT no-op where there is no
  #      server. Both guest libraries had it, identically. Before the fix the
  #      demo mods held ZERO sessions in a focused single-player game while a
  #      bare-Lua mod in that same game delivered 31 datagrams up to 9,188 B
  #      and received inbound events byte-exact.
  #
  # THE TOPOLOGY BELOW STAYS, on better grounds than the ones it had. A
  # headless server with auto_pause=false cannot be stopped by anything a
  # client-side GUI does -- no modal, no menu, no map prompt, no scenario a
  # user loads -- so "game in the background, browser in front" is robust here
  # rather than merely working, and finding (1) is precisely the class of
  # accident it is immune to. The join is also a test nothing else runs: the
  # client is the one peer that executes script.on_load on a world somebody
  # else is already simulating, which is the only place a peer-local guest
  # write shows up. Single player is a real option again, and a simpler one:
  # `--play --single` is that arm, and `--smoke-single` gates it.
  #
  # The client runs the same mods in lockstep -- multiplayer requires it --
  # WITHOUT --enable-lua-udp, and that is measured-safe: a disabled send_udp or
  # recv_udp is a SILENT NO-OP, not a raise (probe, 2026-08-06), so guest state
  # stays byte-identical across the two peers and the CRC holds. Only the
  # server's socket exists; the companion talks to the server; the engine
  # replicates inbound to the client through the ordinary InputAction path.
  RUNMAP="$WORK/ipcdemo-play-map.zip"
  rm -f "$RUNMAP"; cp "$MAP" "$RUNMAP"   # the server SAVES ON EXIT; keep $MAP pristine
  echo
  echo "==> starting the headless server (the simulation that never pauses)"
  rm -f "$USERDIR/factorio-current.log"
  "$FACTORIO" "${CFGARG[@]}" --start-server "$RUNMAP" --mod-directory "$MODDIR" \
    --server-settings "$SETTINGS" --port "$SERVER_PORT" \
    --enable-lua-udp "$GAME_PORT" > "$OUT/play-server.log" 2>&1 &
  GAME_PID=$!
  i=0
  until grep -q "Opening socket" "$OUT/play-server.log" 2>/dev/null; do
    server_alive || { echo "the server died at startup; see $OUT/play-server.log" >&2
                      tail -20 "$OUT/play-server.log" >&2; exit 1; }
    [ $i -lt 120 ] || { echo "server never opened the lua-udp socket; see $OUT/play-server.log" >&2
                        exit 1; }
    sleep 0.5; i=$((i + 1))
  done

  echo "==> starting the companion (GUI mode)"
  "$DEMOBIN" -game-port "$GAME_PORT" -daylight-port "$DAYLIGHT_PORT" \
    -circle-port "$CIRCLE_PORT" -http "$HTTP_ADDR" &
  DEMO_PID=$!
  sleep 0.5
  kill -0 "$DEMO_PID" 2>/dev/null || { echo "the companion exited immediately" >&2; exit 1; }

  URL="http://localhost:${HTTP_ADDR#*:}"
  # THE STEAM RELAUNCH TRAP, and why the client command is shaped the way it is.
  # Launching the Steam build's binary directly makes Steamworks relaunch the
  # game THROUGH Steam, dropping every command-line argument -- the client
  # would come up with the wrong user directory and no mods. Steamworks skips
  # the relaunch when it already knows the appid: steam_appid.txt in the
  # process's working directory plus the SteamAppId/SteamGameId environment
  # variables. 427520 is Factorio. The client gets ITS OWN user directory,
  # because Factorio locks the directory and the server already holds this
  # run's.
  CLIENTDIR="${USERDIR}-client"
  mkdir -p "$CLIENTDIR/config"
  CCFG="$CLIENTDIR/config/config.ini"
  if [ ! -f "$CCFG" ] || ! grep -q "^write-data=$CLIENTDIR\$" "$CCFG"; then
    DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
    if [ -f "$DEFAULT_CFG" ]; then
      sed -e "s|^write-data=.*|write-data=$CLIENTDIR|" "$DEFAULT_CFG" > "$CCFG"
    else
      printf '[path]\nread-data=__PATH__executable__/../data\nwrite-data=%s\n' \
        "$CLIENTDIR" > "$CCFG"
    fi
  fi
  printf '427520\n' > "$CLIENTDIR/steam_appid.txt"
  # Printed rather than only executed, because the useful artifact here is the
  # command line: it is what somebody re-runs tomorrow, pastes into a bug
  # report, or edits to point at their own install.
  CMD=$(printf 'cd %q && SteamAppId=427520 SteamGameId=427520 %q -c %q --mod-directory %q --mp-connect 127.0.0.1:%s' \
        "$CLIENTDIR" "$FACTORIO" "$CCFG" "$MODDIR" "$SERVER_PORT")
  cat <<EOF

  ---------------------------------------------------------------------------
  READY. The world is already running on a headless server on this machine;
  your game connects to it as a player. Two things:

  1. START YOUR GAME with this exact command (or pass --launch and this script
     does it for you). NOT through Steam directly -- Steam will not pass the
     mod directory -- and it lands straight in the world, no menus:

       $CMD

  2. OPEN $URL and drag the sliders.

  What to look for: the Daytime slider drags the sun across the sky and it
  STAYS where you put it (the day/night cycle is frozen). The Radius and Hue
  sliders resize and recolour a circle drawn at spawn, and the "entities
  inside" readout counts what is actually in it -- walk into the circle and
  the count goes up by one. Walk anywhere and the player position readout on
  the left-hand card follows you.

  Alt-tab freely. The simulation is on the server, so nothing your client
  window does can stop it. (Graphical single player is fine too -- measured at
  60 ticks/s unfocused, and at 60 unfocused from birth -- and \`--play --single\`
  is that arm, one process and no join. What the server buys is that no modal
  dialog on your side can pause the world; a freshly created freeplay map opens
  exactly one at tick 750, which the single-player arm removes with a mod.)

  Both mods talk through the server's one socket ($GAME_PORT); each refuses
  the other's datagrams on the source port -- the machinery the --smoke run's
  foreign-port leg proves.

  THE JOIN IS ITSELF A TEST, and it is the only one --smoke cannot run: your
  client is the one peer that runs script.on_load on a world the server is
  already simulating, so anything a mod writes to guest memory from a
  peer-local signal shows up here and nowhere else, as a CRC failure on the
  tick after you join. With --launch this script watches your client's log and
  says so; without it, watch for it yourself in
    $CLIENTDIR/factorio-current.log

  Ctrl-C here when you are done -- it stops the server and the companion.
  ---------------------------------------------------------------------------

EOF
  if [ "$LAUNCH" = 1 ]; then
    echo "==> --launch: starting your game (connects to the local server)"
    rm -f "$CLIENTDIR/factorio-current.log"
    # cd + env: the Steam-relaunch suppression above. A subshell so the
    # companion's wait below is unaffected by the directory change, and `exec`
    # so $! is the GAME's pid: without it stop_all kills a shell that has
    # already forked, and the orphan keeps holding $CLIENTDIR's lock.
    ( cd "$CLIENTDIR" || exit 1
      exec env SteamAppId=427520 SteamGameId=427520 \
        "$FACTORIO" -c "$CCFG" --mod-directory "$MODDIR" \
        --mp-connect "127.0.0.1:$SERVER_PORT" ) &
    CLIENT_PID=$!

    # THE JOIN WATCHER, and it is the only automated thing in --play.
    #
    # A JOINING CLIENT IS THE ONE PEER THAT RUNS script.on_load ON A WORLD
    # SOMEBODY ELSE IS ALREADY SIMULATING, so it is the only place a mod's
    # peer-local state writes show up -- as a CRC failure, on the tick after it
    # joins, repeating. That is what took fkipc's load-reset design down and it
    # is not a thing a headless --smoke run can ever see, because --smoke has
    # one peer. The client writes its own log (its own user directory, because
    # Factorio locks one), so this greps it and says so out loud rather than
    # leaving the operator to notice a stuttering game.
    #
    # It runs for as long as the session does and reports the FIRST occurrence;
    # a desync report is generated by the game either way, next to the log.
    (
      i=0
      while [ $i -lt 10 ] && [ ! -f "$CLIENTDIR/factorio-current.log" ]; do
        sleep 1; i=$((i + 1))
      done
      joined=0
      while kill -0 "$CLIENT_PID" 2>/dev/null; do
        # "Received first update" is 2.0-era and never appears on 2.1.14; the
        # state transition does, and it is what the client log actually says at
        # the moment it starts simulating. Both are accepted rather than the
        # older one being deleted, because this line is the only thing that
        # distinguishes "joined and quiet" from "never got in".
        if [ "$joined" = 0 ] && grep -qE "Received first update|to\(InGame\)" \
             "$CLIENTDIR/factorio-current.log" 2>/dev/null; then
          joined=1
          echo "  [join] the client is in the game and simulating"
        fi
        if grep -q "Multiplayer desynchronisation" \
             "$CLIENTDIR/factorio-current.log" 2>/dev/null; then
          echo "  [join] *** DESYNC -- the client's guest state left the" >&2
          echo "  [join] server's. Something wrote guest memory on one peer;" >&2
          echo "  [join] under --persist=table that memory IS storage.fk_mem" >&2
          echo "  [join] and Factorio CRCs it. Log: $CLIENTDIR/factorio-current.log" >&2
          grep -m3 -n "Multiplayer desynchronisation" \
            "$CLIENTDIR/factorio-current.log" >&2
          exit 0
        fi
        sleep 2
      done
    ) &
  fi
  wait "$DEMO_PID" 2>/dev/null || true
  exit 0
fi

# ---------------------------------------------------------------------------
# --smoke [--single]: automated, named legs. Everything below the game's start
# is shared by the two environments ON PURPOSE -- the point of the single-player
# arm is that the SAME conversation is held against a different topology, so a
# leg worded differently per arm would be a leg that could pass for a different
# reason in each.
# ---------------------------------------------------------------------------
MODS_HERE=("$DAYLIGHT_MOD" "$CIRCLE_MOD")
if [ "$SINGLE" = 1 ]; then
  LOG="$OUT/single-game.log"
  DEMOLOG="$OUT/demo-single.txt"
  RUNMAP="$WORK/ipcdemo-run-single.zip"
  SRCMAP="$SMAP"
  RUNMODDIR="$SMODDIR"
  MODS_HERE+=("$NOINTRO_MOD")
else
  LOG="$OUT/server.log"
  DEMOLOG="$OUT/demo.txt"
  RUNMAP="$WORK/ipcdemo-run.zip"
  SRCMAP="$MAP"
  RUNMODDIR="$MODDIR"
fi
check_path "the run map" "$RUNMAP"

echo
rm -f "$RUNMAP"; cp "$SRCMAP" "$RUNMAP"
rm -f "$USERDIR/factorio-current.log"

if [ "$SINGLE" = 1 ]; then
  echo "==> graphical single-player run -- A GAME WINDOW WILL OPEN, and no human"
  echo "    input is needed: $NOINTRO_MOD removes the one dialog that would ask"
  echo "    for some. Alt-tab away if you like; focus is not a variable."
  start_graphical_game "$RUNMODDIR" "$RUNMAP" "$LOG" --disable-audio
  wait_graphical_ready "$LOG" \
    || { say_fail "game-ready" "the game never reached the map with its socket open"
         stop_game; exit 1; }
  echo "==> letting the game run past the freeplay intro dialog (tick $MODAL_TICK)"
  CLOCK_AT_START=0
  wait_past_modal "$LOG" \
    || { say_fail "sp-clock" "the game stopped short of tick $CLOCK_FLOOR. Tick $MODAL_TICK is where a freeplay map opens its intro dialog, and a modal PAUSES single player until somebody clicks it -- check the $NOINTRO_MOD lines in $LOG"
         stop_game; exit 1; }
else
  echo "==> headless run"
  "$FACTORIO" "${CFGARG[@]}" --start-server "$RUNMAP" --mod-directory "$RUNMODDIR" \
    --server-settings "$SETTINGS" --enable-lua-udp "$GAME_PORT" \
    --disable-audio >"$LOG" 2>&1 &
  GAME_PID=$!

  # Wait for BOTH mods to have loaded before handing the socket over. The
  # companion tolerates a slow start on its own -- each guest re-HELLOs every
  # SearchTicks whether or not anyone is listening -- but a server that died at
  # startup should be reported as that rather than as a silent peer.
  i=0
  while [ $i -lt 120 ]; do
    if grep -q "Checksum for script __${DAYLIGHT_MOD}__" "$LOG" 2>/dev/null \
       && grep -q "Checksum for script __${CIRCLE_MOD}__" "$LOG" 2>/dev/null; then
      break
    fi
    server_alive || break
    sleep 0.5; i=$((i + 1))
  done
  if ! server_alive; then
    echo "    the server EXITED before the mods loaded. See $LOG" >&2
    tail -20 "$LOG" >&2
    say_fail "server" "died before the mods loaded"
    GAME_PID=""
    exit 1
  fi
fi
for m in "${MODS_HERE[@]}"; do
  grep -q "Checksum for script __${m}__" "$LOG" 2>/dev/null \
    || { say_fail "game-ready" "$m never loaded; see $LOG"; stop_game; exit 1; }
done

echo "==> the conversation"
set +e
"$DEMOBIN" -smoke -game-port "$GAME_PORT" -daylight-port "$DAYLIGHT_PORT" \
  -circle-port "$CIRCLE_PORT" -timeout "${SMOKE_TIMEOUT}s" >"$DEMOLOG" 2>&1 &
DEMO_PID=$!
wait "$DEMO_PID"
rc=$?
set -e
DEMO_PID=""
sed 's/^/    /' "$DEMOLOG"
[ "$rc" -eq 0 ] || FAILED=1

if ! server_alive; then
  echo "    *** THE GAME DIED DURING THE CONVERSATION -- that is the" >&2
  echo "    TickClosure.cpp:91 crash shape. See $LOG" >&2
  grep -inE "invalid input action|TickClosure\.cpp|Factorio crashed|non-recoverable" \
    "$LOG" | head -5 >&2 || true
  say_fail "survival" "the game did not survive the session"
  GAME_PID=""
  exit 1
fi

# The guest-side half, which only the guest can say. One second is ~60 ticks and
# each guest acts on a BYE in the pump that receives it.
sleep 1
GAME_WAS="$GAME_PID"
stop_game

echo
for pair in "daylight:$DAYLIGHT_MOD" "circle:$CIRCLE_MOD"; do
  key="${pair%%:*}"; mod="${pair#*:}"
  lines="$(guest_lines "$LOG" "$mod")"
  printf '%s\n' "$lines" | sed "s/^/    $key: /"
  printf '%s\n' "$lines" > "$OUT/guest-$key.txt"
  ups="$(printf '%s\n' "$lines" | grep -c 'fkipc session up' || true)"
  # EXACTLY TWO, and the number is the IDENTITY LEG read from the game's own
  # side. A companion binds one socket per mod port, so proving a token mismatch
  # against a live game means closing the correctly-paired session and opening a
  # deliberately mismatched one on the same port -- see identityLeg in
  # sdk/go/cmd/ipcdemo/smoke.go. So the run has three phases and the guest sees:
  #
  #   1. up   -- the matched companion, which every leg above the identity one uses
  #   2. down -- that companion's BYE when the identity leg takes the port
  #      ...and NOTHING here, which is the assertion: for the whole mismatched
  #      phase the guest was offered a token it was not built against and refused
  #      to adopt one. A third up would be the guest adopting an ACK from an
  #      application it does not know.
  #   3. up   -- the matched pairing restored, which is the positive control
  #
  # A count of 1 is the identity leg's own third arm never recovering; 3 or more
  # is the guest adopting something it should have refused; 0 is no session at
  # all.
  case "$ups" in
    2) say_pass "session-guest-$key" "the guest adopted exactly two sessions -- one before the identity leg and one after it, and NONE from the mismatched companion in between" ;;
    *) say_fail "session-guest-$key" "$ups session-up lines, want 2 (matched, then the identity leg's mismatched phase with none, then matched again) -- more means it adopted a token it should have refused, or reloaded; fewer means the matched pairing never came back" ;;
  esac
  # THE POSITIVE CONTROL FOR THE FOREIGN-PORT LEG, and it is why that leg is a
  # discriminator rather than an observation that nothing happened. The
  # companion sent a BYE at the live epoch from a THIRD socket and this guest
  # ignored it; the companion then sent the SAME FRAME TYPE at the SAME EPOCH
  # from its configured port when it closed, and the guest tore the session
  # down. Same frame, different source port, opposite outcome.
  if printf '%s\n' "$lines" | grep -q 'fkipc session down'; then
    say_pass "bye-guest-$key" "a BYE from the configured port DID tear the session down"
  else
    say_fail "bye-guest-$key" "no session-down line: the companion's own BYE never reached the guest, so the foreign-port leg proves nothing"
  fi
done

# ---------------------------------------------------------------------------
# The single-player arm's own legs. Everything above is the same conversation
# the headless gate holds; these three are what only this environment can say.
# ---------------------------------------------------------------------------
if [ "$SINGLE" = 1 ]; then
  echo
  # 1. THE CLOCK, READ FROM THE GAME AND NOT FROM fkipc, and this is the modal
  #    regression detector. The wait before the conversation already proved the
  #    game got PAST tick 750 -- a dialog there stops single player dead, which
  #    would have made every leg above fail with fkipc looking like the culprit.
  #    What is left to say is that it kept going while the conversation happened,
  #    because a game that reached tick 900 and then stopped is a different
  #    failure with the same first half. fk-demo-nointro is bare Lua with no wasm
  #    in it, so both halves are evidence about the ENGINE's clock rather than
  #    about fkipc's reading of it.
  last="$(nointro_ticks "$LOG" | tail -1)"
  if [ "${last:-0}" -le "$CLOCK_AT_START" ]; then
    say_fail "sp-clock" "the game's own clock was tick $CLOCK_AT_START before the conversation and tick ${last:-0} after it: it stopped while the session was live. Check the $NOINTRO_MOD lines in $LOG"
  else
    say_pass "sp-clock" "the game's own clock passed the tick-$MODAL_TICK intro dialog that would have paused single player, and ran on from $CLOCK_AT_START to $last across the conversation"
  fi

  # 2. THE GUEST'S clock, which is a different fact from the game's: the game
  #    can be ticking while the guest is deaf. guest_tick is carried by the
  #    HEARTBEAT, which is unconditional, so it is the peer's reading of the
  #    guest's own game.tick -- a session that came up and then stopped being
  #    pumped shows a game clock that advances and a guest clock that does not.
  gticks="$(sed -n 's/.*guest_tick=\([0-9][0-9]*\).*/\1/p' "$DEMOLOG")"
  gn="$(printf '%s\n' "$gticks" | grep -c . || true)"
  gmin="$(printf '%s\n' "$gticks" | sort -n | head -1)"
  if [ "$gn" -lt 2 ]; then
    say_fail "sp-guest-clock" "only $gn STATS line(s) in $DEMOLOG: both mods should report one"
  elif [ "${gmin:-0}" -lt "$CLOCK_FLOOR" ]; then
    say_fail "sp-guest-clock" "a guest's clock reached only tick $gmin, short of $CLOCK_FLOOR: the game may be running while a guest is no longer being pumped"
  else
    say_pass "sp-guest-clock" "both guests' clocks reached at least tick $gmin in their heartbeats, so the pump ran for the whole session"
  fi

  # 3. TEARDOWN. A graphical run leaves a WINDOW behind if this is wrong, and an
  #    orphaned Factorio holds the user directory's lock -- which the operations
  #    notes describe as "dies at startup and reads as a broken gate" for every
  #    later in-game run here. It is asserted rather than assumed because the
  #    single-player arm is the first thing in this script whose game is started
  #    through a subshell, and `exec` is what makes $! the game's own pid.
  left=""
  ! kill -0 "$GAME_WAS" 2>/dev/null || left="$GAME_WAS"
  for p in $(pgrep -x factorio 2>/dev/null || true); do
    ps -o command= -p "$p" 2>/dev/null | grep -qF "$RUNMODDIR" && left="$left $p"
  done
  # The lock itself, where the platform leaves a file to look at. Skipped rather
  # than failed when there is none: what releases it is the process exiting, and
  # that is what the check above establishes.
  lock="$USERDIR/.lock"
  lockheld=""
  if [ -f "$lock" ]; then
    python3 - "$lock" >/dev/null 2>&1 <<'PY' || lockheld=" and $USERDIR/.lock is still held"
import fcntl, sys
with open(sys.argv[1], "a") as f:
    fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
PY
  fi
  if [ -n "$left" ] || [ -n "$lockheld" ]; then
    say_fail "sp-teardown" "factorio survived the run (pid(s):$left)$lockheld -- the next run of any in-game gate here would die at startup on the user directory lock"
  else
    say_pass "sp-teardown" "the game is gone and $USERDIR is free again"
  fi
fi

echo
echo "Transcript: $DEMOLOG, game log: $LOG"
[ "$FAILED" -eq 0 ] || { echo "==> FAILED" >&2; exit 1; }
echo "==> done"
