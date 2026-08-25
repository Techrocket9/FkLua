#!/usr/bin/env bash
# Build the M4 guest, package it, and run it inside a real Factorio, headlessly.
#
# This is the only check that the mod actually LOADS. lua52f models Factorio's
# sandbox and the end-to-end test drives control.lua against stand-ins for the
# game globals, but neither one is Factorio: the mod format, the `require`
# resolution, the size of the chunk the game will parse and the log plumbing are
# all outside what the oracle can speak to.
#
# Creates a throwaway map with only the guest mod enabled, runs it for a few
# hundred ticks, and greps the guest's own log lines back out.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"
TICKS="${TICKS:-120}"
# The optimization level to package at. The mod has to load and run at every
# level, and only this script can say whether it does.
#
# UNSET means "whatever fklua defaults to". Pinning a number here is how the
# script quietly stops testing the default: -opt=3 became the default at M7 and
# a hardcoded 2 would have gone on passing without ever compiling it.
OPT="${OPT:-}"

# Which example to run. `hello` is the M4 guest -- arithmetic, memory and the
# log import. `api` is the M7 one, and the only thing here that crosses the
# host-call ABI, so a break in dispatch or marshalling shows up in `api` alone.
# LANG picks the toolchain. `hello` exists in both, and the two are written as
# line-for-line mirrors so their output can be compared -- which is what
# TestBothToolchainsAgree does under the oracle. This is the same guest in the
# real game.
LANG_="${LANG_:-go}"
GUEST="${GUEST:-hello}"
case "$GUEST" in
  hello) MODNAME=fk-hello; INIT_RE="hello from (Go|Rust)|fnv64"; RUN_RE="(tick [0-9]+ seen=)" ;;
  goroutine) MODNAME=fk-gor; INIT_RE="goroutines:|pipeline sum:"
         RUN_RE="(tick [0-9]+ goroutines-run=)"; WASI=1 ;;
  api)   MODNAME=fk-api;   INIT_RE="reaching Factorio|defines\\.direction\\.east = 4"
         # The last two are the closeout round's binding shapes, and they are in
         # this pattern because a line nobody prints is a line nobody checks:
         # both ran green in game for two milestones' worth of runs before
         # anyone looked in the raw log to find out.
         RUN_RE="(game\\.speed|game\\.tick|event: on_tick #|no string crossed|reused buffer|surfaces= |chunk operator|inventory operators|index-assign|last-error|global-fn|multi-return)" ;;
  # The collected guest. Its job here is the one thing no host-side test can
  # do: prove a mod whose guest COLLECTS ITS OWN HEAP loads and runs in the
  # real game. It logs `intact=32/32` alongside the tick counter, so a
  # collection that reclaimed a live block shows up in the log rather than as
  # a mod that quietly computes the wrong thing.
  gcsave) MODNAME=fk-gcsave; INIT_RE="\\[gcsave\\] .* collector ON"
         RUN_RE="(tick [0-9]+ seen=.*intact=32)"; GC=collected ;;
  # `array` is deliberately NOT here. It needs a connected player to have any
  # handle to work with, and a headless benchmark has none -- it would report
  # "handles: 0", return early and pass while testing nothing. Its home is
  # TestArraysCrossInBothDirections, where a stub supplies the objects.
  *) echo "unknown GUEST: $GUEST (want hello, api, goroutine or gcsave)" >&2; exit 1 ;;
esac

MODDIR="$ROOT/testdata/tmp/guest-mods"
TMPDIR="$ROOT/testdata/tmp"
MAP="$TMPDIR/guest-map.zip"
WASM="$TMPDIR/$GUEST.wasm"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO" >&2; echo "set FACTORIO_BIN" >&2; exit 1; }
# THE INSTALLED ENGINE'S SERIES, read once. Every mod this script packages --
# generated and hand-written alike -- declares it, because info.json's
# factorio_version is a claim about the ENGINE and the API pin is a claim about
# the DESCRIPTION, and the two default apart now that the pin is GA. A 2.1
# engine refuses a mod declaring 2.0 at game start. See scripts/lib-engine.sh.
SERIES="$(factorio_series)"

# A PRIVATE WRITE-DATA DIRECTORY, so this can run while a Factorio is already
# open. Factorio LOCKS its user directory, and a second process pointed at the
# same one dies at startup -- which reads as a broken gate rather than as two
# copies of the game. Setting FACTORIO_USERDIR to somewhere of your own is not
# enough on its own: the path in the environment only tells this script where to
# read logs from, and the GAME needs -c with a config.ini whose write-data says
# the same thing.
CFGARG=()
if [ -n "${FACTORIO_USERDIR:-}" ]; then
  mkdir -p "$USERDIR/config"
  CFG="$USERDIR/config/config.ini"
  if [ ! -f "$CFG" ]; then
    DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
    if [ -f "$DEFAULT_CFG" ]; then
      # The installed config, with write-data redirected. Copying it rather than
      # writing a minimal one keeps read-data whatever this install actually
      # uses, which is not guessable from the executable's path on every
      # platform.
      sed -e "s|^write-data=.*|write-data=$USERDIR|" "$DEFAULT_CFG" > "$CFG"
    else
      printf '[path]\nread-data=__PATH__executable__/../data\nwrite-data=%s\n' "$USERDIR" > "$CFG"
    fi
  fi
  CFGARG=(-c "$CFG")
fi

command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
command -v wasm-opt >/dev/null || { echo "wasm-opt is not installed: brew install binaryen" >&2; exit 1; }

mkdir -p "$TMPDIR"
rm -rf "$MODDIR" "$MAP"
mkdir -p "$MODDIR"

if [ "$LANG_" = "rust" ]; then
  case "$GUEST" in hello|api|gcsave) ;; *) echo "GUEST=$GUEST has no Rust example" >&2; exit 1 ;; esac
  command -v cargo >/dev/null || { echo "cargo is not installed: https://rustup.rs" >&2; exit 1; }
  # THE COLLECTOR IS A --features FLAG AND NOT A -gc FLAG, and it is passed on the
  # command line rather than declared in the guest's Cargo.toml: Cargo's v2
  # resolver unifies features across a workspace build, so a declared one would
  # turn the collector on for every other example too.
  #
  # This arm used to ignore $GC entirely, so `GC=collected LANG_=rust` built a
  # leaking guest and handed --gc=collected to `fklua mod` -- which refused it,
  # correctly, for carrying no collector surface.
  RSFEAT=""
  [ "${GC:-leaking}" = collected ] && RSFEAT="--features fk/fkgc"
  echo "==> building the guest with Rust (gc=${GC:-leaking})"
  # One command, no post-processing: FkLua compiles memory.copy/fill natively,
  # so a stock build goes straight through.
  # A SEPARATE TARGET DIR PER ARM: cargo writes both to the same artifact path,
  # so sharing one silently ships whichever was built last.
  ( cd "$ROOT/guest/rust" && CARGO_TARGET_DIR="target/${GC:-leaking}" \
      cargo build --release --target wasm32-unknown-unknown -p "$GUEST" $RSFEAT )
  cp "$ROOT/guest/rust/target/${GC:-leaking}/wasm32-unknown-unknown/release/$GUEST.wasm" "$WASM"
  MODNAME="$MODNAME-rs"
else
  if [ "${WASI:-0}" = 1 ]; then
    # wasip1 for goroutines. -buildmode=c-shared is not optional: wasip1
    # defaults to a COMMAND exporting _start, which runs main and terminates,
    # and a mod needs a REACTOR exporting _initialize.
    echo "==> building the guest with TinyGo (wasip1, asyncify)"
    ( cd "$ROOT/guest/go" && tinygo build -target=wasip1 -buildmode=c-shared \
        -o "$WASM" ./examples/$GUEST )
  else
    # -gc=custom for the collected guest, and it is not optional: the flag is
    # only half of it, and a build without the guest/go/fkgc import does not
    # link at all.
    TGGC=leaking; [ "${GC:-leaking}" = collected ] && TGGC=custom
    echo "==> building the guest with TinyGo (-gc=$TGGC)"
    ( cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none "-gc=$TGGC" -opt=2 \
        -o "$WASM" ./examples/$GUEST )
  fi
fi

echo "==> packaging"
go run "$ROOT/cmd/fklua" mod "$WASM" ${OPT:+--opt="$OPT"} --gc="${GC:-leaking}" \
  --factorio-version "$SERIES" \
  --name "$MODNAME" --version 0.1.0 --author FkLua \
  --description "FkLua end-to-end guest ($GUEST)" \
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
  >"$TMPDIR/guest-create.log" 2>&1 \
  || { echo "map creation failed; see $TMPDIR/guest-create.log" >&2
       tail -40 "$TMPDIR/guest-create.log" >&2; exit 1; }

# on_init fires while the map is being created, so its output is in THIS log,
# not the benchmark's.
grep -E "$INIT_RE" "$TMPDIR/guest-create.log" || true

# Two runs of the same save, not one. Factorio replays it from identical state
# both times, so the guest's own output has to come back identical -- which is
# the cheapest real determinism check available here, and the only one that runs
# the generated Lua inside the actual lockstep simulation. A guest that is
# nondeterministic desyncs every client in a multiplayer game, and nothing in
# the host-side test suite can see that.
RUNS="${RUNS:-2}"
echo "==> running $TICKS ticks x $RUNS"
"$FACTORIO" "${CFGARG[@]}" --mod-directory "$MODDIR" \
            --benchmark "$MAP" \
            --benchmark-ticks "$TICKS" \
            --benchmark-runs "$RUNS" \
            --disable-audio >"$TMPDIR/guest-run.log" 2>&1 \
  || { echo "run failed; see $TMPDIR/guest-run.log" >&2
       tail -40 "$TMPDIR/guest-run.log" >&2; exit 1; }

echo "==> guest output"
if ! grep -E "$RUN_RE" "$TMPDIR/guest-run.log"; then
  echo "the guest logged nothing; see $TMPDIR/guest-run.log" >&2
  tail -40 "$TMPDIR/guest-run.log" >&2
  exit 1
fi

if [ "$RUNS" -gt 1 ]; then
  echo "==> determinism across $RUNS runs"
  # Every distinct guest line must appear exactly $RUNS times. A line seen a
  # different number of times means the runs disagreed about it.
  #
  # ...EXCEPT AN ENGINE-RENDERED PROFILER DURATION, WHICH IS A WALL CLOCK AND
  # CANNOT REPEAT. `log{"", "...", p}` on a LuaProfiler is the only way to read
  # one at all -- the class exposes no accessor for the number -- and what the
  # engine writes is `Duration: 0.007208ms`, a real elapsed time that differs
  # between two runs of identical work. Masking the digits is not weakening the
  # check: the LINE still has to appear $RUNS times, in the same place, with the
  # same text either side of the figure.
  #
  # It is also the guest-author rule stated as a filter. A duration is
  # peer-local by construction, so a guest may LOG one -- the game log is not
  # CRC'd, which is why fk.Log is the sanctioned sink for anything per-peer --
  # and may never BRANCH on one. A guest that did would desync every client, and
  # this harness would not see it.
  odd=$(grep -oE "$RUN_RE.*" "$TMPDIR/guest-run.log" \
        | sed -E 's/Duration: [0-9.]+[a-z]*/Duration: <elapsed>/' \
        | sort | uniq -c | awk -v n="$RUNS" '$1 != n')
  if [ -n "$odd" ]; then
    echo "the guest was NOT deterministic across runs:" >&2
    echo "$odd" >&2
    exit 1
  fi
  echo "    all guest lines identical across $RUNS runs"
fi

echo "==> done"
