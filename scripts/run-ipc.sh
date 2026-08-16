#!/usr/bin/env bash
# The fkipc live gate: a real conversation with a real Factorio, over the real
# socket, in both guest languages.
#
# scripts/run-ipcprobe.sh is the first thing in this repo that talks to a
# running Factorio; THIS IS THE FIRST THAT SPEAKS THE PROTOCOL AT ONE. The probe
# was a measurement, taken once, and its findings are in agents/ipc.md. This is
# a standing gate: build the example guest, package it, start a headless server
# with --enable-lua-udp, and hold one full fkipc session against it -- handshake,
# an RPC carrying all 256 byte values, telemetry, the guest's own CLOCK seen to
# advance in its heartbeats, a RESYNC answered with a SNAPSHOT, a file picked up
# and verified against its digest, EXACTLY ONE SESSION for the whole run, and a
# clean BYE.
#
# THE SESSION COUNT IS A VERDICT NOW, and that is where P6 is buried. Starting a
# headless server LOADS the map, and while a load reset the guest's session that
# put two HELLOs on the wire one tick apart -- so a companion that happened to
# bind early enough minted two sessions and failed anything in flight. Whether a
# run saw it was a race, which is why the count used to sit in the STATS line
# where nothing compared it. A load resets nothing today, so the count is
# determined and the companion asserts it.
#
# WHAT ONLY THIS CAN SAY. The conformance suite drives the shipping guest state
# machine against the shipping SDK over an in-memory link, and internal/guest
# drives both compiled guests through the verbatim control.lua under lua52f. So
# the protocol is not what is under test here. What is under test is everything
# between them and the game: that recv_udp delivers on the installed build at
# all, that a LocalisedString payload survives the engine in both directions,
# that a guest's write_file lands where the SDK looks for it, that the pruned
# event table still routes on_udp_packet_received, and that a headless server
# ticks at all -- which is auto_pause, below, and is the single most likely way
# for this whole thing to look broken when it is not.
#
#   FACTORIO_USERDIR=/tmp/fkuser ./scripts/run-ipc.sh
#   LANG_=rust ./scripts/run-ipc.sh
#   RUNS=1 ./scripts/run-ipc.sh          # skip the determinism comparison
#
# THE ENGINE FLOOR IS REAL AND THIS SCRIPT CANNOT WORK AROUND IT. On 2.0.77 a
# headless recv_udp with a packet queued kills the server at TickClosure.cpp:91
# -- a C++ abort no pcall can catch. Below MinEngineVersion = 2.1.14 the guest
# library is HARD-DISABLED: no HELLO, no pump, not one datagram. So this gate
# REFUSES TO START on an older install rather than running: every leg would sit
# at its deadline and the run would report a protocol failure for an engine that
# is simply too old, which is the reported-red twin of the skip-that-reads-as-a-
# pass this repo has been bitten by twice. The floor is read out of the library,
# not written here; see scripts/lib-engine.sh.
#
# THE FLOOR IS ABOUT THE ENGINE AND NOT ABOUT THE API PIN, which is a separate
# axis: the pin decides which description the bindings came from and defaults to
# the general-availability release, while fkipc asks helpers.game_version at run
# time. A GA-pinned mod gets the whole library on a 2.1.14 engine, which is
# exactly what this gate packages and runs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
WORK="$ROOT/testdata/tmp"
OUT="$WORK/ipc"
# A PRIVATE USERDIR BY DEFAULT, which follows run-ipcprobe.sh rather than
# run-guest.sh and for its reason: this script runs for a minute with a socket
# open, and Factorio LOCKS its user directory, so defaulting to the installed
# one makes "somebody had the game open" the most likely outcome of a first run.
USERDIR="${FACTORIO_USERDIR:-$WORK/ipc-userdir}"

GAME_PORT="${GAME_PORT:-25409}"   # --enable-lua-udp: the game's one socket
# ...and the companion's, which MUST differ. 25411 is not a free choice: it is
# compiled into guest/{go,rust}/examples/ipc as Config.Port, because a guest
# has no configuration file. Overriding this without editing the example gives
# a guest talking to a port nobody is listening on.
GATE_PORT="${GATE_PORT:-25411}"
LANG_="${LANG_:-go}"
# TWO RUNS, and the comparison between them is a deliverable rather than a
# formality -- see "determinism" at the bottom for what is and is not comparable
# in a protocol whose session id is entropy the companion minted.
RUNS="${RUNS:-2}"
# How long the companion may hold the wire, in seconds, passed straight to its
# own -timeout. It matters because the script WAITS on it: a companion with no
# deadline would hold a headless Factorio open indefinitely, and an orphaned
# server locks the user directory for every later in-game run. Six legs at the
# companion's 20 s per-leg deadline is the worst case this has to clear.
GATE_TIMEOUT="${GATE_TIMEOUT:-120}"

case "$LANG_" in
  go)   MODNAME=fk-ipc ;;
  rust) MODNAME=fk-ipc-rs ;;
  *) echo "unknown LANG_: $LANG_ (want go or rust)" >&2; exit 1 ;;
esac

# EVERYTHING DERIVED IS PER LANGUAGE, including the map. A save records the mods
# it was made with, so a map created with fk-ipc and loaded with fk-ipc-rs is a
# missing-mod error -- and the cached-map branch below would otherwise hand the
# second arm the first arm's map and report it as the mod failing to load.
MAP="$WORK/ipc-map-$LANG_.zip"
MODDIR="$WORK/ipc-mods-$LANG_"
WASM="$WORK/ipc-$LANG_.wasm"
GATEBIN="$WORK/ipcgate"

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

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"
[ "$GAME_PORT" != "$GATE_PORT" ] || {
  echo "GAME_PORT and GATE_PORT must differ: --enable-lua-udp binds one socket" >&2
  echo "  and that port is also the SOURCE port of everything the game sends." >&2
  exit 1; }

# THE BACKGROUNDED FACTORIO DIES WITH THE SCRIPT THAT STARTED IT. An orphaned
# server holds the user directory lock, which is the state the operations notes
# describe as "dies at startup and reads as a broken gate" for every later
# in-game run, until somebody finds the process by hand.
#
# The EXIT trap does NOT exit and the signal traps MUST -- an exiting EXIT trap
# replaces this script's own status, and a TERM trap that does not exit SWALLOWS
# the signal and lets the script carry on against a game it has just killed.
# Same shape as run-roundtrip.sh and run-ipcprobe.sh, for the same measured
# reason. AND THE KILL ESCALATES, because a Factorio has taken minutes to act on
# a plain SIGTERM here and nothing downstream can tell that from a hung harness.
GAME_PID=""
GATE_PID=""
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
  [ -n "$GATE_PID" ] && kill "$GATE_PID" 2>/dev/null || true
  return 0
}
trap stop_all EXIT
trap 'stop_all; exit 130' INT
trap 'stop_all; exit 143' TERM

mkdir -p "$WORK" "$OUT" "$USERDIR"

# Factorio LOCKS its user directory, so a run started while a game is open dies
# at startup. Pointing FACTORIO_USERDIR somewhere is not enough on its own --
# that only tells this script where to READ logs -- so the config.ini the GAME
# is handed has to say the same write-data. Copied from the installed one rather
# than written from scratch, because read-data is not guessable from the
# executable's path on every platform.
#
# The staleness check matters here for the same reason it does in the probe: the
# default userdir is under testdata/tmp, and a stale config left by an earlier
# ROOT would point write-data at a directory this run never looks in -- which
# would also send the guest's script-output file somewhere the companion is not
# watching, i.e. the bulk leg failing for a reason that is not about bulk.
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

# Both ports free before anything binds them. A companion whose port is already
# taken fails in a way that looks exactly like "the game never replied", which
# is the finding this script exists to establish.
python3 - "$GAME_PORT" "$GATE_PORT" <<'PY' || exit 1
import socket, sys
for p in sys.argv[1:]:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.bind(("127.0.0.1", int(p)))
    except OSError as e:
        print(f"UDP port {p} on 127.0.0.1 is not free: {e}", file=sys.stderr)
        print("  Set GAME_PORT / GATE_PORT, or stop whatever holds it.", file=sys.stderr)
        sys.exit(1)
    finally:
        s.close()
PY

# auto_pause IS THE LOAD-BEARING KEY, and it is the probe's finding rather than
# a copied default. A headless server with nobody connected PAUSES: the update
# loop stops, on_tick never fires, the pump never runs and recv_udp is never
# called -- which reads exactly like "UDP does not work on this build". The rest
# is run-roundtrip.sh's settings file with autosave effectively off, because
# this gate wants no save at all.
SETTINGS="$WORK/ipc-server-settings.json"
cat > "$SETTINGS" <<'JSON'
{ "name": "fkipc", "description": "", "visibility": { "public": false, "lan": false },
  "username": "", "password": "", "token": "",
  "require_user_verification": false, "max_upload_in_kilobytes_per_second": 0,
  "minimum_latency_in_ticks": 0, "ignore_player_limit_for_returning_players": false,
  "allow_commands": "true", "autosave_interval": 1000000, "autosave_slots": 5,
  "afk_autokick_interval": 0, "auto_pause": false, "only_admins_can_pause_the_game": true,
  "autosave_only_on_server": true, "non_blocking_saving": false }
JSON

echo "==> Factorio $("$FACTORIO" --version 2>/dev/null | head -1 | sed 's/Version: //')"
# EARLY, BEFORE ANYTHING IS BUILT OR STARTED. Below the floor the sessions can
# never come up, so there is nothing to learn from letting the run proceed.
require_fkipc_floor
echo "==> lang $LANG_, mod $MODNAME, game udp $GAME_PORT, companion $GATE_PORT"
echo "==> userdir $USERDIR"

# ---------------------------------------------------------------------------
# Build. THE WASM IS ALWAYS REBUILT, for run-roundtrip.sh's recorded reason: a
# cached guest cost sharding stage C four wrong conclusions in a row, each fix
# re-run against the binary the first invocation had built.
# ---------------------------------------------------------------------------
if [ "$LANG_" = rust ]; then
  command -v cargo >/dev/null || { echo "cargo is not installed: https://rustup.rs" >&2; exit 1; }
  echo "==> building guest/rust/examples/ipc"
  rm -f "$WASM"
  ( cd "$ROOT/guest/rust" && CARGO_TARGET_DIR="$WORK/cargo-ipc" \
      cargo build --release --target wasm32-unknown-unknown -p ipc )
  cp "$WORK/cargo-ipc/wasm32-unknown-unknown/release/ipc.wasm" "$WASM"
else
  command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
  command -v wasm-opt >/dev/null || { echo "wasm-opt is not installed: brew install binaryen" >&2; exit 1; }
  echo "==> building guest/go/examples/ipc"
  rm -f "$WASM"
  ( cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none \
      -gc=leaking -opt=2 -o "$WASM" ./examples/ipc )
fi
[ -s "$WASM" ] || { echo "the toolchain produced no $WASM" >&2; exit 1; }

echo "==> packaging"
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"
rm -rf "$MODDIR"; mkdir -p "$MODDIR"
# --opt UNSET means "whatever fklua defaults to", which is the point: a pinned
# level here is how a script quietly stops testing the default.
"$ROOT/bin/fklua" mod "$WASM" --name "$MODNAME" --version 0.1.0 --author FkLua \
  --factorio-version "$SERIES" \
  --description "FkLua IPC live gate ($LANG_)" -o "$MODDIR" | sed 's/^/    /'
cat > "$MODDIR/mod-list.json" <<JSON
{"mods":[{"name":"base","enabled":true},{"name":"$MODNAME","enabled":true}]}
JSON

# THE COMPANION IS BUILT FROM THE SDK MODULE, as an outside tool would build it.
# That is a gate in itself: sdk/go is a separate module whose only dependency on
# this repo is guest/go/fkipc/wire, and a `//go:wasmimport` leaking into that
# package would fail here rather than in somebody's build.
echo "==> building the companion from sdk/go"
( cd "$ROOT/sdk/go" && go build -o "$GATEBIN" ./cmd/ipcgate )

# ---------------------------------------------------------------------------
# One throwaway map, created WITH the mod so a mod that fails to load fails
# here rather than inside a measured window. EVERY RUN GETS ITS OWN COPY: a
# headless server SAVES THE MAP WHEN IT STOPS, so a map handed to two runs in
# turn starts the second one later than the first -- which the probe learned by
# losing an arm to it, and which here would mean run 2 seeing a different tick
# sequence than run 1 and the determinism comparison failing for a reason that
# is not about the guest.
# ---------------------------------------------------------------------------
if [ ! -f "$MAP" ] || [ -n "${FRESH:-}" ]; then
  echo "==> creating throwaway map"
  rm -f "$MAP"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" --create "$MAP" \
    --map-gen-seed 1 --disable-audio >"$OUT/create-$LANG_.log" 2>&1 \
    || { echo "map creation failed; see $OUT/create-$LANG_.log" >&2
         tail -30 "$OUT/create-$LANG_.log" >&2; exit 1; }
fi
grep -q "Checksum for script __${MODNAME}__" "$OUT/create-$LANG_.log" \
  || { echo "the mod did not load during --create: every run below would be" >&2
       echo "  the BASE GAME. See $OUT/create-$LANG_.log" >&2; exit 1; }

FAILED=0
say_pass() { printf 'PASS %s -- %s\n' "$1" "$2"; }
say_fail() { printf 'FAIL %s -- %s\n' "$1" "$2"; FAILED=1; }

server_alive() { [ -n "$GAME_PID" ] && kill -0 "$GAME_PID" 2>/dev/null; }

# The guest's own lines, out of the captured server log. NOT deduped across two
# channels the way the probe does it: the ORDER and the COUNT of these are the
# determinism assertion, and `awk '!seen[$0]++'` would collapse a genuine repeat
# into one.
guest_lines() { sed -n 's/.*\(fkipc session .*\)$/\1/p' "$1"; }

# ---------------------------------------------------------------------------
# One run: start the server, hold the conversation, stop the server, and read
# the guest's side of it back out of the log.
# ---------------------------------------------------------------------------
run_once() {
  local n="$1"
  local log="$OUT/server-$LANG_-$n.log"
  local gatelog="$OUT/gate-$LANG_-$n.txt"
  local map="$WORK/ipc-map-$LANG_-$n.zip"
  check_path "the run-$n map" "$map"

  echo
  echo "==> run $n/$RUNS"
  rm -f "$map"; cp "$MAP" "$map"
  # A STALE FILE WOULD PASS THE BULK LEG WITHOUT THE GUEST WRITING ANYTHING.
  # The payload is deterministic, so last run's copy has the right length and
  # the right digest -- which is exactly why it has to go.
  rm -f "$USERDIR/script-output/fkipc-gate.bin"
  rm -f "$USERDIR/factorio-current.log"

  "$FACTORIO" "${CFGARG[@]}" --start-server "$map" --mod-directory "$MODDIR" \
    --server-settings "$SETTINGS" --enable-lua-udp "$GAME_PORT" \
    --disable-audio >"$log" 2>&1 &
  GAME_PID=$!

  # Wait for the mod to have LOADED before handing the socket over. The
  # companion tolerates a slow start on its own -- the guest re-HELLOs every
  # SearchTicks whether or not anyone is listening -- but a server that died at
  # startup should be reported as that rather than as a silent peer.
  local i=0
  while [ $i -lt 120 ]; do
    grep -q "Checksum for script __${MODNAME}__" "$log" 2>/dev/null && break
    server_alive || break
    sleep 0.5; i=$((i + 1))
  done
  if ! server_alive; then
    echo "    the server EXITED before the mod loaded. See $log" >&2
    tail -20 "$log" >&2
    say_fail "server" "died before the mod loaded"
    GAME_PID=""
    return 1
  fi
  if ! grep -q "Checksum for script __${MODNAME}__" "$log" 2>/dev/null; then
    say_fail "server" "the mod never loaded in 60 s; see $log"
    stop_game
    return 1
  fi

  # THE CONVERSATION.
  set +e
  "$GATEBIN" -game-port "$GAME_PORT" -listen-port "$GATE_PORT" \
    -script-output "$USERDIR/script-output" \
    -timeout "${GATE_TIMEOUT}s" >"$gatelog" 2>&1 &
  GATE_PID=$!
  wait "$GATE_PID"
  local rc=$?
  set -e
  GATE_PID=""
  sed 's/^/    /' "$gatelog"
  [ "$rc" -eq 0 ] || FAILED=1

  if ! server_alive; then
    echo "    *** THE SERVER DIED DURING THE CONVERSATION -- that is the" >&2
    echo "    TickClosure.cpp:91 crash shape. See $log" >&2
    grep -inE "invalid input action|TickClosure\.cpp|Factorio crashed|non-recoverable" \
      "$log" | head -5 >&2 || true
    say_fail "survival" "the server did not survive the session"
    GAME_PID=""
    return 1
  fi

  # The BYE's GUEST-SIDE half. The companion can only say it sent one; whether
  # the guest tore the session down is a fact about the guest, and the guest
  # says so in the log. One second is ~60 ticks, and the guest acts on a BYE in
  # the pump that receives it.
  sleep 1
  stop_game

  local lines; lines="$(guest_lines "$log")"
  printf '%s\n' "$lines" | sed 's/^/    guest: /'
  printf '%s\n' "$lines" > "$OUT/guest-$LANG_-$n.txt"
  case "$(printf '%s\n' "$lines" | grep -c 'fkipc session up')" in
    1) say_pass "session-guest" "the guest adopted exactly one session" ;;
    *) say_fail "session-guest" "the guest logged $(printf '%s\n' "$lines" | grep -c 'fkipc session up') session-up lines, want 1" ;;
  esac
  if printf '%s\n' "$lines" | grep -q 'fkipc session down'; then
    say_pass "bye-guest" "the guest saw the BYE and tore the session down"
  else
    say_fail "bye-guest" "no session-down line: the BYE did not reach the guest"
  fi
  return 0
}

for n in $(seq 1 "$RUNS"); do
  run_once "$n" || true
done

# ---------------------------------------------------------------------------
# DETERMINISM, and what is honestly assertable here is narrower than it is
# anywhere else in this repo.
#
# run-guest.sh compares two --benchmark runs LINE FOR LINE, because a replay of
# one save is the same computation twice. This is not that. Two things in an
# fkipc session are entropy or a race and neither is a defect:
#
#   * THE EPOCH IS PEER-MINTED. The guest cannot mint a unique session id --
#     that is a theorem, not a limitation: everything it can compute is a
#     deterministic function of state that time-travels with the save. So the
#     companion draws the token from real entropy and every frame in the
#     session carries it. Two runs' frames differ in four bytes by design.
#   * THE TICK A DATAGRAM LANDS ON IS A RACE between the companion's real clock
#     and the game's update loop, so seq numbers, the tick in a telemetry
#     payload and the number of heartbeats in a window all move run to run.
#
# What IS deterministic is the guest's STATE PROGRESSION given the same inbound
# sequence -- one session adopted, one torn down by a BYE, in that order -- and
# the verdict of each leg. So that is what is compared: the guest's own session
# lines, and the leg names with their PASS/FAIL, with the details (which carry
# the epoch and the tick) deliberately dropped.
#
# Those two lines used to be three: a `reset` came first, because loading the
# map ran fk_after_load and the library reset its session there. It does not any
# more -- fk_after_load fires on a joining multiplayer client and on no other
# peer, so a library that wrote guest state there desynced the joiner -- and a
# run whose transcript shows `reset` is a run against a build from before that.
# ---------------------------------------------------------------------------
if [ "$RUNS" -gt 1 ]; then
  echo
  echo "==> determinism across $RUNS runs"
  for n in $(seq 2 "$RUNS"); do
    if [ ! -f "$OUT/guest-$LANG_-1.txt" ] || [ ! -f "$OUT/guest-$LANG_-$n.txt" ]; then
      say_fail "determinism-guest" "a run produced no guest output to compare"
      continue
    fi
    if diff -u "$OUT/guest-$LANG_-1.txt" "$OUT/guest-$LANG_-$n.txt" >"$OUT/diff-guest-$LANG_-$n.txt"; then
      say_pass "determinism-guest" "run $n's guest lines are identical to run 1's"
    else
      say_fail "determinism-guest" "run $n's guest lines differ from run 1's"
      sed 's/^/    /' "$OUT/diff-guest-$LANG_-$n.txt" >&2
    fi
    # The verdicts, not the details: `PASS session` is comparable and
    # `epoch 0x51c0ffee` is not.
    awk '/^(PASS|FAIL) /{print $1, $2}' "$OUT/gate-$LANG_-1.txt" >"$OUT/verdicts-$LANG_-1.txt"
    awk '/^(PASS|FAIL) /{print $1, $2}' "$OUT/gate-$LANG_-$n.txt" >"$OUT/verdicts-$LANG_-$n.txt"
    if diff -u "$OUT/verdicts-$LANG_-1.txt" "$OUT/verdicts-$LANG_-$n.txt" >"$OUT/diff-verdicts-$LANG_-$n.txt"; then
      say_pass "determinism-legs" "run $n reached the same verdict on every leg"
    else
      say_fail "determinism-legs" "run $n's leg verdicts differ from run 1's"
      sed 's/^/    /' "$OUT/diff-verdicts-$LANG_-$n.txt" >&2
    fi
  done
fi

echo
echo "Transcripts: $OUT/gate-$LANG_-*.txt, server logs: $OUT/server-$LANG_-*.log"
[ "$FAILED" -eq 0 ] || { echo "==> FAILED" >&2; exit 1; }
echo "==> done"
