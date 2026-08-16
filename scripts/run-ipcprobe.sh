#!/usr/bin/env bash
# What helpers.send_udp / helpers.recv_udp / on_udp_packet_received ACTUALLY do,
# on the installed Factorio, measured by talking to a running one.
#
# THIS IS THE FIRST THING IN THIS REPO THAT TALKS TO A LIVE FACTORIO. Every
# other in-game harness here starts a game, lets it run, and reads the log
# afterwards. This one holds a socket open while the game is up, so the two
# halves have to be started in the right order and neither may be left behind.
#
# THE GATING QUESTION, AND ITS ANSWER. A headless `recv_udp(0)` crashed the game
# at 2.1.9 -- "Trying to add invalid input action to the closure",
# TickClosure.cpp:91 -- and whether the 2.0.77 build this repo pins had that
# defect was unestablished either way. IT DOES. The first run of this script
# killed the server at map tick 31 with that exact signature, and it has
# reproduced on every attempt since, at the same tick from a fresh map. It is a
# C++ crash and not a Lua error, so no pcall in the mod can catch it: what says
# it happened is the server process being gone, which `server_alive` checks for
# and reports as a FINDING rather than as a broken harness.
#
# WHAT THE MATRIX BELOW ESTABLISHED, on 2.0.77, so that a later reader does not
# have to re-derive it:
#
#   * the crash needs BOTH a recv_udp call AND a packet in the socket. Calling
#     the pump on an empty socket is fine for as long as you like, and packets
#     piling up in a socket nobody reads are fine too.
#   * it is recv_udp(0) and recv_udp() -- the two forms that read for the
#     SERVER. recv_udp(1), which reads for a player who does not exist, is
#     safe and delivers nothing.
#   * minimum_latency_in_ticks does not matter; 0 and 6 both crash.
#   * send_udp is entirely healthy, in every LocalisedString form, including
#     binary, and does not crash anything.
#   * under --benchmark nothing is delivered at all -- no crash and no events --
#     because there is neither a server nor a player for recv_udp to read FOR.
#     `players=0 connected=0 multiplayer=false` there, against
#     `multiplayer=true` on the server.
#
# THE ARMS, and each earns its place by being the control for another.
#
#   server         headless --start-server, pump = recv_udp(0), the mod answers
#                  everything. The real question, and the only arm that can
#                  hold a conversation.
#   benchmark      --benchmark, pump = recv_udp(0). Whether a future gate can
#                  skip the server dance -- and `for_player = 0` means "the
#                  server if present", which in a benchmark there is not.
#   benchmark-bare --benchmark, pump = recv_udp() with no argument, which is a
#                  different call and the obvious suspect if the arm above
#                  receives nothing.
#
# AND THE CRASH MATRIX, which exists because the `server` arm CRASHED THE GAME
# on the first run of this script and the stack trace names a path both halves
# of the API can reach. A crash report that says "UDP crashes the server" is
# worth much less than one that says which call does it, so the four cells vary
# the two things independently -- whether the mod PUMPS, and whether anything
# is SENT in either direction:
#
#   crash-none      no pump, no send, no inbound. --enable-lua-udp and nothing
#                   else. The null control: if this dies, the flag alone does it.
#   crash-sendonly  send_udp on a schedule, no pump, nothing inbound.
#   crash-recvidle  recv_udp(0) every tick on an EMPTY socket, nothing sent.
#   crash-recvpkt   recv_udp(0) every tick with inbound packets arriving, and
#                   the mod answers none of them (`seq` is the one command with
#                   no reply), so nothing outbound contaminates the cell.
#   crash-recvpkt-bare / -lat / -one   the same cell with the pump argument, the
#                   server's minimum latency, and the read-for-player-1 form
#                   varied one at a time.
#   server-nopump   inbound traffic at a server that never pumps.
#
# AND THE OUTBOUND ARM, which is where the send half is measured. The mod runs
# its own send schedule off on_tick and the driver only listens, so the game
# never receives anything and never crashes -- which is the only way to measure
# sending in the environment the server profile actually uses.
#
#   FACTORIO_USERDIR=/tmp/fkipc ./scripts/run-ipcprobe.sh
#   ARMS="outbound" ./scripts/run-ipcprobe.sh
#   ARMS="crash-none crash-sendonly crash-recvidle crash-recvpkt" ./scripts/run-ipcprobe.sh
#
# --enable-lua-udp binds ONE socket, and that port is both the game's receive
# socket and the source port of everything it sends, so the driver must listen
# on a different one. Both are settable; both are checked to be free first,
# because send_udp to an occupied port crashed the game before 2.0.61 and
# nothing guarantees the receive side is more forgiving.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
WORK="$ROOT/testdata/tmp"
OUT="$WORK/ipcprobe"
# A PRIVATE USERDIR BY DEFAULT, which is the opposite of every other script
# here and is deliberate: this one runs for minutes with a socket open, and
# Factorio LOCKS its user directory, so defaulting to the installed one makes
# "somebody had the game open" the most likely outcome of a first run. Set
# FACTORIO_USERDIR to override; the config.ini branch below is identical either
# way, because a private write-data needs -c whatever chose it.
USERDIR="${FACTORIO_USERDIR:-$WORK/ipcprobe-userdir}"

GAME_PORT="${GAME_PORT:-28613}"      # --enable-lua-udp: the game's one socket
DRIVER_PORT="${DRIVER_PORT:-28614}"  # ...and the driver's, which must differ
BURST="${BURST:-20}"
LATENCY_SAMPLES="${LATENCY_SAMPLES:-20}"
BENCH_SECONDS="${BENCH_SECONDS:-20}"
BENCH_TICKS="${BENCH_TICKS:-200000}"
# How long a crash-matrix cell holds the wire open. The observed crash landed at
# map tick 31, about 2.5 s after the server reached InGame, so 25 s is ~1,500
# ticks of margin -- long enough that "survived" means something.
CELL_SECONDS="${CELL_SECONDS:-25}"

MAP="$WORK/ipcprobe-map.zip"

# Every path here is interpolated into a Factorio command line and some into
# JSON. A newline in one is how sharding stage C created three directories
# NAMED BY SHELL OUTPUT, which rode into a commit with nothing failing. Refuse
# rather than quote harder.
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

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO (set FACTORIO_BIN)" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"
[ "$GAME_PORT" != "$DRIVER_PORT" ] || {
  echo "GAME_PORT and DRIVER_PORT must differ: --enable-lua-udp binds one socket" >&2
  echo "  and that port is also the SOURCE port of everything the game sends." >&2
  exit 1; }

# THE BACKGROUNDED FACTORIO DIES WITH THE SCRIPT THAT STARTED IT. An orphaned
# server holds the user directory lock, which is the state the operations notes
# describe as "dies at startup and reads as a broken gate" for every later
# in-game run, until somebody finds the process by hand.
#
# The EXIT trap does NOT exit and the signal traps MUST -- an exiting EXIT trap
# replaces this script's own status, and a TERM trap that does not exit
# SWALLOWS the signal and lets the script carry on against a game it has just
# killed. Same shape as run-roundtrip.sh, for the same measured reason.
#
# AND THE KILL ESCALATES. A `--benchmark` Factorio took SIX MINUTES to act on a
# plain SIGTERM here -- the driver had finished at 20 s and the arm sat in
# `wait` until the game felt like closing its socket. Nothing downstream can
# tell that apart from a hung harness, so TERM gets a few seconds and then the
# process is killed outright.
GAME_PID=""
DRIVER_PID=""
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
  [ -n "$DRIVER_PID" ] && kill "$DRIVER_PID" 2>/dev/null || true
  return 0
}
trap stop_all EXIT
trap 'stop_all; exit 130' INT
trap 'stop_all; exit 143' TERM

mkdir -p "$WORK" "$OUT" "$USERDIR"

# Factorio LOCKS its user directory, so a run started while a game is open dies
# at startup. Pointing FACTORIO_USERDIR somewhere is not enough on its own --
# that only tells this script where to READ logs -- so the config.ini the GAME
# is handed has to say the same write-data. Copied from the installed one
# rather than written from scratch, because read-data is not guessable from the
# executable's path on every platform: a hand-written guess cost a run
# elsewhere with "there is no package core".
#
# The staleness check matters here more than in the other scripts, because the
# default userdir is under testdata/tmp and a stale config left by an earlier
# GAME_PORT or an earlier ROOT would point write-data at a directory this run
# never looks in.
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

# Both ports free before anything binds them. send_udp to an OCCUPIED port
# crashed the game before 2.0.61 and this build is past that, but a driver
# whose port is already taken fails in a way that looks exactly like "the game
# never replied", which is the finding this whole script exists to establish.
python3 - "$GAME_PORT" "$DRIVER_PORT" <<'PY' || exit 1
import socket, sys
for p in sys.argv[1:]:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.bind(("127.0.0.1", int(p)))
    except OSError as e:
        print(f"UDP port {p} on 127.0.0.1 is not free: {e}", file=sys.stderr)
        print("  Set GAME_PORT / DRIVER_PORT, or stop whatever holds it.", file=sys.stderr)
        sys.exit(1)
    finally:
        s.close()
PY

arm_dir() { printf '%s/ipcprobe-mods-%s' "$WORK" "$1"; }
arm_log() { printf '%s/ipcprobe-%s.log' "$OUT" "$1"; }
arm_map() { printf '%s/ipcprobe-map-%s.zip' "$WORK" "$1"; }

# EVERY ARM GETS ITS OWN COPY OF THE MAP, and that is not tidiness. A headless
# server SAVES THE MAP WHEN IT STOPS, so a map handed to four arms in turn
# starts each one later than the last -- 0, then 1556, then 3116. It cost an arm
# before it was noticed: `crash-sendonly` was scheduled to send at tick 30 on a
# map that opened at 1556, so the cell that was supposed to prove send_udp safe
# proved only that it was never called, and it reported "survived" either way.
arm_fresh_map() {
  local m; m="$(arm_map "$1")"
  check_path "the $1 map path" "$m"
  rm -f "$m"; cp "$MAP" "$m"
}

# The mod is one directory per arm because config.lua differs. Everything else
# in it is the committed probe, copied verbatim.
build_arm() {
  local name="$1" pump="$2" form="${3:-concat}" fp="${4:-0}" autosend="${5:-30}"
  local dumptick="${6:-0}" suite="${7:-false}"
  local dir; dir="$(arm_dir "$name")"
  check_path "the $name mod directory" "$dir"
  rm -rf "$dir"; mkdir -p "$dir"
  cp -R "$ROOT/testdata/ipcprobe/fklua-ipcprobe" "$dir/fklua-ipcprobe_0.0.1"
  stamp_series "$dir/fklua-ipcprobe_0.0.1"
  cat > "$dir/fklua-ipcprobe_0.0.1/config.lua" <<LUA
-- GENERATED by scripts/run-ipcprobe.sh for arm "$name". Do not edit.
return { reply_port = $DRIVER_PORT, pump = "$pump", for_player = $fp,
         send_form = "$form", autosend_after = $autosend, dump_after = $dumptick,
         outbound_suite = $suite }
LUA
  cat > "$dir/mod-list.json" <<'JSON'
{"mods":[{"name":"base","enabled":true},{"name":"fklua-ipcprobe","enabled":true}]}
JSON
}

# auto_pause IS THE LOAD-BEARING KEY. A headless server with nobody connected
# PAUSES by default: the update loop stops, on_tick never fires, the pump never
# runs and recv_udp is never called -- which reads exactly like "UDP does not
# work on this build", the wrong answer to the one question this script exists
# to ask. The roundtrip script's settings are the template; this key is the
# addition.
#
# minimum_latency_in_ticks IS A VARIABLE HERE, and it is the second suspect in
# the crash rather than a knob. The crash is `InputActionSegmenter` handing an
# action to a `TickClosure` that rejects it, which is the multiplayer latency
# machinery -- and 0 ticks of latency means the closure an action is destined
# for may already have been sent. The value copied in from run-roundtrip.sh is
# 0, so a run that only ever used that value could not tell "UDP receive is
# broken" from "the latency setting this harness happened to inherit".
server_settings() {
  local minlat="$1"
  local out="$WORK/ipcprobe-server-settings-$minlat.json"
  cat > "$out" <<JSON
{ "name": "fkipc", "description": "", "visibility": { "public": false, "lan": false },
  "username": "", "password": "", "token": "",
  "require_user_verification": false, "max_upload_in_kilobytes_per_second": 0,
  "minimum_latency_in_ticks": $minlat, "ignore_player_limit_for_returning_players": false,
  "allow_commands": "true", "autosave_interval": 1000000, "autosave_slots": 5,
  "afk_autokick_interval": 0, "auto_pause": false, "only_admins_can_pause_the_game": true,
  "autosave_only_on_server": true, "non_blocking_saving": false }
JSON
  printf '%s' "$out"
}

# The mod's lines out of BOTH channels -- the captured stdout and
# factorio-current.log -- deduped by exact line rather than by `sort -u`,
# because ORDER is one of the findings and the lines carry their own monotone
# counter for exactly this.
probe_lines() {
  cat "$1" "$USERDIR/factorio-current.log" 2>/dev/null \
    | sed -n 's/.*\(FKIPCPROBE .*\)/\1/p' | awk '!seen[$0]++'
}

server_alive() { [ -n "$GAME_PID" ] && kill -0 "$GAME_PID" 2>/dev/null; }

# Did the game die, and does the log say the 2.1.9 way? Called after every arm;
# a crash is the single most valuable thing this script can report.
crash_report() {
  local log="$1" name="$2"
  # TickClosure.cpp WITH THE EXTENSION, because `nextTickClosureTick(0)` is an
  # ordinary Info line every server prints and a bare `TickClosure` matched it --
  # so every healthy arm reported a crash signature until this was tightened.
  if grep -qiE "invalid input action|TickClosure\.cpp|Factorio crashed|The mod .* caused a non-recoverable error" "$log"; then
    echo "    *** $name: the log carries a CRASH signature:" >&2
    grep -inE "invalid input action|TickClosure\.cpp|Factorio crashed|non-recoverable" "$log" | head -5 >&2
    return 1
  fi
  return 0
}

echo "==> Factorio $("$FACTORIO" --version 2>/dev/null | head -1 | sed 's/Version: //')"
echo "==> game udp port $GAME_PORT, driver port $DRIVER_PORT, userdir $USERDIR"

# ---------------------------------------------------------------------------
# One throwaway map, created WITH the mod so its on_init (there is none, but a
# mod that fails to load fails here) is outside every measured window. Reused
# across arms because the map is a pure function of the seed; FRESH=1 rebuilds.
# ---------------------------------------------------------------------------
build_arm mapgen zero
if [ ! -f "$MAP" ] || [ -n "${FRESH:-}" ]; then
  echo "==> creating throwaway map"
  rm -f "$MAP"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$(arm_dir mapgen)" --create "$MAP" \
    --map-gen-seed 1 --disable-audio >"$OUT/create.log" 2>&1 \
    || { echo "map creation failed; see $OUT/create.log" >&2
         tail -30 "$OUT/create.log" >&2; exit 1; }
  grep -q "Checksum for script __fklua-ipcprobe__" "$OUT/create.log" \
    || { echo "the probe mod did not load during --create -- every arm below" >&2
         echo "  would be the BASE GAME. See $OUT/create.log" >&2; exit 1; }
fi

FAILED=0
CRASHED=0
# One line per arm, printed at the end. The crash matrix is only readable as a
# table, and a table assembled from four separate scrollbacks is not one.
MATRIX=""
note_arm() { MATRIX="$MATRIX$(printf '    %-16s %s' "$1" "$2")
"; }

# ---------------------------------------------------------------------------
# A headless-server arm: start the game, wait for the mod's own READY line,
# hand the socket to the driver, stop the game.
# ---------------------------------------------------------------------------
run_server_arm() {
  local name="$1" pump="$2" dmode="${3:-server}" autosend="${4:-30}" hs="${5:-90}"
  local minlat="${6:-0}" fp="${7:-0}" suite="${8:-false}"
  local dir log settings
  dir="$(arm_dir "$name")"; log="$(arm_log "$name")"
  settings="$(server_settings "$minlat")"
  check_path "the $name mod directory" "$dir"

  echo "==> $name: headless server, pump=$pump, driver=$dmode, autosend=+$autosend, minlat=$minlat, fp=$fp"
  build_arm "$name" "$pump" concat "$fp" "$autosend" 0 "$suite"
  arm_fresh_map "$name"
  rm -f "$USERDIR/factorio-current.log"
  "$FACTORIO" "${CFGARG[@]}" --start-server "$(arm_map "$name")" --mod-directory "$dir" \
    --server-settings "$settings" \
    --enable-lua-udp "$GAME_PORT" --disable-audio >"$log" 2>&1 &
  GAME_PID=$!

  # Wait for the mod's first tick rather than for a Factorio banner: READY is
  # the mod saying the update loop is running, which is what auto_pause=false
  # is there to make true and is not implied by the server having started.
  local i=0
  while [ $i -lt 120 ]; do
    probe_lines "$log" | grep -q "READY" && break
    server_alive || break
    sleep 0.5; i=$((i + 1))
  done
  if ! server_alive; then
    echo "    *** the server EXITED before the mod ever ticked. See $log" >&2
    tail -20 "$log" >&2
    crash_report "$log" "$name" || true
    note_arm "$name" "DIED BEFORE THE FIRST TICK"
    GAME_PID=""; FAILED=1; return 1
  fi
  if ! probe_lines "$log" | grep -q "READY"; then
    echo "    *** no READY line in 60 s: the update loop is not running." >&2
    echo "    auto_pause is the usual cause; it is false in the settings above." >&2
    tail -20 "$log" >&2
    stop_game
    note_arm "$name" "NEVER TICKED (auto_pause?)"
    FAILED=1; return 1
  fi
  echo "    $(probe_lines "$log" | grep -m1 READY)"

  python3 "$ROOT/scripts/ipc-probe-driver.py" \
    --game-port "$GAME_PORT" --listen-port "$DRIVER_PORT" --mode "$dmode" \
    --burst "$BURST" --latency-samples "$LATENCY_SAMPLES" \
    --seconds "$CELL_SECONDS" \
    --handshake-timeout "$hs" --out "$OUT/$name.json" || true

  if ! server_alive; then
    echo "    *** THE SERVER DIED DURING THE PROBE. This is the crash the 2.1.9" >&2
    echo "    report describes, on this build. See $log" >&2
    echo "    Map tick at the crash: $(sed -n 's/.*Map tick at moment of crash: \([0-9]*\).*/\1/p' "$log" | head -1)" >&2
    crash_report "$log" "$name" || true
    note_arm "$name" "CRASHED at map tick $(sed -n 's/.*Map tick at moment of crash: \([0-9]*\).*/\1/p' "$log" | head -1), $(sed -n 's/.*Error \(TickClosure.cpp:[0-9]*\).*/\1/p' "$log" | head -1)"
    GAME_PID=""; CRASHED=1
  else
    sleep 0.5
    stop_game
    note_arm "$name" "survived ${CELL_SECONDS}s, $(probe_lines "$log" | grep -c ' EV ' || true) event(s)"
  fi

  probe_lines "$log" > "$OUT/$name.probe.txt" || true
  cp "$USERDIR/factorio-current.log" "$OUT/$name.factorio.log" 2>/dev/null || true
  echo "    $(wc -l < "$OUT/$name.probe.txt" | tr -d ' ') FKIPCPROBE lines -> $OUT/$name.probe.txt"
  crash_report "$log" "$name" || CRASHED=1
}

# ---------------------------------------------------------------------------
# A --benchmark arm: the game runs the update loop as fast as it can, so
# nothing can converse with it. The DRIVER STARTS FIRST -- tick 30 arrives
# within milliseconds of the load, and a listener started afterwards would miss
# the one unprompted send.
# ---------------------------------------------------------------------------
run_benchmark_arm() {
  local name="$1" pump="$2" dmode="${3:-benchmark}" fp="${4:-0}" # $5 = outbound suite
  local dir log; dir="$(arm_dir "$name")"; log="$(arm_log "$name")"
  check_path "the $name mod directory" "$dir"

  echo "==> $name: --benchmark, pump=$pump, driver=$dmode, fp=$fp (driver first: +30 ticks is immediate)"
  # dump_after writes the JSON without being asked, because this arm may never
  # be reachable by a `dump` command.
  build_arm "$name" "$pump" concat "$fp" 30 120 "${5:-false}"
  arm_fresh_map "$name"
  rm -f "$USERDIR/factorio-current.log"

  python3 "$ROOT/scripts/ipc-probe-driver.py" \
    --game-port "$GAME_PORT" --listen-port "$DRIVER_PORT" --mode "$dmode" \
    --seconds "$BENCH_SECONDS" --burst "$BURST" \
    --latency-samples "$LATENCY_SAMPLES" --handshake-timeout 30 \
    --out "$OUT/$name.json" &
  DRIVER_PID=$!
  sleep 0.5

  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$(arm_map "$name")" \
    --benchmark-ticks "$BENCH_TICKS" --benchmark-runs 1 \
    --enable-lua-udp "$GAME_PORT" --disable-audio >"$log" 2>&1 &
  GAME_PID=$!

  wait "$DRIVER_PID" 2>/dev/null || true
  DRIVER_PID=""
  if server_alive; then
    stop_game
  else
    echo "    (the benchmark had already finished or died)"
    GAME_PID=""
  fi

  grep -q "Checksum for script __fklua-ipcprobe__" "$log" \
    || { echo "    *** the mod never loaded in this arm; nothing below is about it" >&2
         FAILED=1; }
  probe_lines "$log" > "$OUT/$name.probe.txt" || true
  cp "$USERDIR/factorio-current.log" "$OUT/$name.factorio.log" 2>/dev/null || true
  echo "    $(wc -l < "$OUT/$name.probe.txt" | tr -d ' ') FKIPCPROBE lines -> $OUT/$name.probe.txt"
  local nev; nev="$(grep -c ' EV ' "$OUT/$name.probe.txt" || true)"
  echo "    events seen: $nev"
  if crash_report "$log" "$name"; then
    note_arm "$name" "survived, $nev event(s), $(sed -n 's/.*Performed \([0-9]*\) updates.*/\1/p' "$log" | head -1) ticks"
  else
    note_arm "$name" "CRASHED"
    CRASHED=1
  fi
}

# THE DEFAULT LIST IS THE WHOLE PUBLISHED MATRIX, in the order the findings
# document reads. `server` is first because it is the gating question, and it
# is expected to crash: a run where it does NOT is the news.
ARMLIST="${ARMS:-server outbound crash-none crash-sendonly crash-recvidle \
crash-recvpkt crash-recvpkt-bare crash-recvpkt-lat crash-recvpkt-one \
server-nopump benchmark benchmark-bare benchmark-omit}"
for name in $ARMLIST; do
  case "$name" in
    #                              arm            pump driver  autosend hs minlat fp
    server)         run_server_arm server         zero server    30 90 || true ;;
    server-bare)    run_server_arm server-bare    bare server    30 60 || true ;;
    server-lat)     run_server_arm server-lat     zero server    30 90  6 || true ;;
    # The control, and its handshake timeout is short ON PURPOSE: it is
    # SUPPOSED to time out. A no-pump arm that answers means something other
    # than recv_udp is delivering packets.
    server-nopump)  run_server_arm server-nopump  none server     0 20 || true ;;
    # The crash matrix. Nothing here converses, so `hs` is unused and the
    # driver simply holds the wire open for CELL_SECONDS.
    crash-none)     run_server_arm crash-none     none silent     0  1 || true ;;
    crash-sendonly) run_server_arm crash-sendonly none silent    30  1 || true ;;
    crash-recvidle) run_server_arm crash-recvidle zero silent     0  1 || true ;;
    crash-recvpkt)  run_server_arm crash-recvpkt  zero blast      0  1 || true ;;
    # ...and the two ways out of it that cost one flag each to test. The pump
    # ARGUMENT (recv_udp() rather than recv_udp(0)) is a different call into the
    # same machinery; minimum_latency_in_ticks is the setting the crashing stack
    # is about, inherited as 0 from the roundtrip script.
    crash-recvpkt-bare) run_server_arm crash-recvpkt-bare bare blast 0 1 || true ;;
    crash-recvpkt-lat)  run_server_arm crash-recvpkt-lat  zero blast 0 1 6 || true ;;
    # recv_udp(1) reads for PLAYER 1, who does not exist on a server nobody has
    # joined. If this is the one pump argument that survives, the failure is
    # about WHICH INSTANCE is being read for rather than about receiving.
    crash-recvpkt-one)  run_server_arm crash-recvpkt-one  one  blast 0 1 || true ;;
    # THE OUTBOUND SUITE, on a real headless server. Nothing is sent AT the
    # game, so it never receives and never crashes, and the mod runs the whole
    # send schedule off its own on_tick. This is where answers 3, 4 and 5 come
    # from for the server profile.
    outbound)       run_server_arm outbound none outbound 0 1 0 0 true || true ;;
    outbound-omit)  run_server_arm outbound-omit none outbound 0 1 0 -1 true || true ;;
    bench-outbound) run_benchmark_arm bench-outbound none outbound -1 true || true ;;
    benchmark)      run_benchmark_arm benchmark      zero benchmark || true ;;
    benchmark-bare) run_benchmark_arm benchmark-bare bare benchmark || true ;;
    # for_player OMITTED rather than 0. `0` means "the server if present", and a
    # --benchmark has no server -- so the arm above cannot distinguish "UDP is
    # dead in a benchmark" from "this argument selected nobody".
    benchmark-omit) run_benchmark_arm benchmark-omit bare benchmark -1 || true ;;
    # The FULL SUITE against a --benchmark game rather than a server. The
    # headless server cannot survive an inbound packet on this build, so if
    # this arm converses it is the only place answers 2 through 7 exist.
    bench-suite)    run_benchmark_arm bench-suite    zero server    || true ;;
    bench-suite-bare) run_benchmark_arm bench-suite-bare bare server -1 || true ;;
    *) echo "unknown arm: $name" >&2; FAILED=1 ;;
  esac
done

# script-output, wherever the write_file legs landed it.
if [ -d "$USERDIR/script-output/fkipc" ]; then
  rm -rf "$OUT/script-output"
  cp -R "$USERDIR/script-output/fkipc" "$OUT/script-output" 2>/dev/null || true
  echo "==> script-output: $(ls "$OUT/script-output" 2>/dev/null | tr '\n' ' ')"
else
  echo "==> script-output: nothing under $USERDIR/script-output/fkipc"
fi

echo
echo "==> survival matrix (the answer to whether this API is usable at all)"
printf '%s\n' "$MATRIX"

echo
echo "==> summary"
for name in $ARMLIST; do
  f="$OUT/$name.json"
  [ -f "$f" ] || continue
  python3 - "$f" "$name" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
name = sys.argv[2]
hs = r.get("handshake")
if hs is not None:
    print(f"    {name}: handshake={'OK' if hs.get('ok') else 'NO REPLY'} "
          f"attempts={hs.get('attempts')} source_port={hs.get('source_port')}")
bm = r.get("benchmark")
if bm is not None:
    print(f"    {name}: sent {bm['sent_pairs']} pairs, {bm['replies']} reply datagram(s), "
          f"first={bm['first_reply']}")
for k in ("binary_inbound", "binary_outbound", "burst", "latency", "for_player",
          "sizes_inbound", "sizes_outbound", "source_port", "localised_string_forms"):
    if k in r:
        print(f"      {k}: {json.dumps(r[k])}")
PY
done

echo
echo "Full per-arm records: $OUT/<arm>.json, $OUT/<arm>.probe.txt"
# A CRASH IS THE FINDING AND NOT A FAILURE, which is why it does not set the
# exit status. On 2.0.77 the crash cells are SUPPOSED to crash -- a run where
# they stopped doing so would be the news -- so an exit code that went red on
# the expected outcome would train a reader to ignore it. What does go red is
# the harness not working: a mod that did not load, a server that never ticked,
# a map that could not be made. The matrix above is the result.
if [ "$CRASHED" -ne 0 ]; then
  echo "NOTE: at least one arm CRASHED THE GAME. On 2.0.77 that is the expected" >&2
  echo "  result for an inbound packet on a headless server -- read the matrix." >&2
fi
[ "$FAILED" -eq 0 ] || {
  echo "ONE OR MORE ARMS DID NOT RUN -- that is a harness failure, not a finding." >&2
  exit 1; }
