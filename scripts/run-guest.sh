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
# Lines whose VALUE this guest must log, as opposed to a pattern that only says
# the guest spoke at all. ONE EXTENDED REGEX PER LINE, all of which have to
# match; empty for every guest that has none. See the check after the run, which
# is where the reason it exists is written down.
MUST_RE=""
case "$GUEST" in
  hello) MODNAME=fk-hello; INIT_RE="hello from (Go|Rust)|fnv64"; RUN_RE="(tick [0-9]+ seen=)" ;;
  goroutine) MODNAME=fk-gor; INIT_RE="goroutines:|pipeline sum:"
         RUN_RE="(tick [0-9]+ goroutines-run=)"; WASI=1 ;;
  # The named-subscription line is here because examples/api subscribes to a
  # custom input whose prototype it deliberately does not define, so what this
  # reaches is the ENGINE's own refusal arriving as one log line with the mod
  # still loading. The SUCCESS path needs a data stage and is
  # scripts/run-custominput.sh's.
  api)   MODNAME=fk-api;   INIT_RE="reaching Factorio|defines\\.direction\\.east = 4|refused the event name"
         # The last two are the closeout round's binding shapes, and they are in
         # this pattern because a line nobody prints is a line nobody checks:
         # both ran green in game for two milestones' worth of runs before
         # anyone looked in the raw log to find out.
         # `shorthand struct` is the one leg here no stub can stand in for: a
         # `table | tuple` concept is sent by the ENGINE in whichever form it
         # chooses, and Vector's own description says it always chooses the
         # array. A stub returns whatever it was written to return, so this is
         # where the descriptor's pos= flag is checked against a real Factorio.
         # It read 0.00,0.00 with status OK before that flag existed.
         #
         # `inserter_drop_position` is in the pattern BESIDE it because only
         # TWO of the leg's branches say `shorthand struct`. Counted in the
         # source rather than remembered: the leg has SIX branches, an if/else-if
         # chain in guest/go/examples/api/main.go and the same six arms written
         # as a nested match in guest/rust/examples/api/src/lib.rs. Two of them
         # say `shorthand struct` (the value and the absent case); the member's
         # own failure branch logs `inserter_drop_position failed:`, which
         # without this token would vanish from the output entirely rather than
         # reporting; and the THREE branches above that one name
         # `prototypes.entity` instead. `entity\.position` is in the pattern for
         # the SAME reason one member over: the second read's success line says
         # `keyed struct:` and matches no other alternative, and its failure
         # branch logs `entity.position failed:`, so without the token the whole
         # keyed half would be missing from the determinism comparison instead of
         # reported. Those three are covered by MUST_RE below
         # rather than here -- a line that is simply GONE is what a presence grep
         # cannot see.
         RUN_RE="(game\\.speed|game\\.tick|event: on_tick #|no string crossed|reused buffer|surfaces= |chunk operator|inventory operators|shorthand struct|inserter_drop_position|entity\\.position|bulk: |index-assign|last-error|global-fn|multi-return)"
         # THE TWO VALUES IN THIS SCRIPT THAT ARE ASSERTED RATHER THAN READ,
         # and they are one measurement in two halves. Base's own inserter drops
         # at (0, 1.2) and the chest the guest builds stands at (8.5, 8.5), and
         # the whole point of the leg is the NUMBERS: a positional read that
         # regressed would print a perfectly well-formed 0.00,0.00.
         #
         # The second line is the OTHER FORM of the same shape. Vector and
         # MapPosition are both `table | tuple` concepts and both carry pos=,
         # and the engine sends the first as an array and the second keyed --
         # which is the per-member choice this whole change rests on. Asserting
         # both is what makes the run a measurement of it rather than a
         # transcription: the array form has to keep reading through the
         # fallback, and the keyed form has to keep reading without it.
         MUST_RE="shorthand struct: inserter_drop_position = 0\\.00,1\\.20
keyed struct: entity\\.position = 8\\.50,8\\.50" ;;
  # The collected guest. Its job here is the one thing no host-side test can
  # do: prove a mod whose guest COLLECTS ITS OWN HEAP loads and runs in the
  # real game. It logs `intact=32/32` alongside the tick counter, so a
  # collection that reclaimed a live block shows up in the log rather than as
  # a mod that quietly computes the wrong thing.
  gcsave) MODNAME=fk-gcsave; INIT_RE="\\[gcsave\\] .* collector ON"
         RUN_RE="(tick [0-9]+ seen=.*intact=32)"; GC=collected ;;
  # The TYPED ARGUMENT form of a member whose parameter table is a
  # discriminated union. The only thing here that crosses fk.call_typed, so a
  # break in M.call_typed or in either generator's typed encode shows up in
  # `typedargs` alone.
  #
  # ITS GUI HALF CANNOT RUN HERE AND SAYS SO. LuaGuiElement::add needs a player
  # and a headless --create has none, so what this reaches is create_entity --
  # the other variant-defeated member, on a surface that exists at tick 0. The
  # `gui: no player` line is in the pattern deliberately: a guest that silently
  # skipped a leg would be a run that proved less than it looks.
  typedargs) MODNAME=fk-typedargs
         INIT_RE="gui: no player|entity dyn: iron-chest|entity typed: iron-chest"
         RUN_RE="(tick typed: iron-chest)" ;;
  # `array` is deliberately NOT here. It needs a connected player to have any
  # handle to work with, and a headless benchmark has none -- it would report
  # "handles: 0", return early and pass while testing nothing. Its home is
  # TestArraysCrossInBothDirections, where a stub supplies the objects.
  *) echo "unknown GUEST: $GUEST (want hello, api, typedargs, goroutine or gcsave)" >&2; exit 1 ;;
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
  case "$GUEST" in hello|api|typedargs|gcsave) ;; *) echo "GUEST=$GUEST has no Rust example" >&2; exit 1 ;; esac
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

# A PRESENCE GREP OVER AN ALTERNATION CANNOT SEE ONE MISSING LINE, and neither
# can the determinism check below, which only counts what did appear. So a leg
# that vanished, or that logged a well-formed WRONG NUMBER, passes both: with
# fk_abi.lua's positional fallback deleted this script printed `shorthand
# struct: inserter_drop_position = 0.00,0.00`, reported "all guest lines
# identical", and exited 0 -- which is exactly the defect that leg exists to
# catch. Writing the expected value down is what closes that.
#
# If a future base version moves the inserter's drop position, this fails and
# names the number it wanted. That is the right outcome for a figure the ABI's
# own documentation quotes: it is a fact about the engine, and a fact about the
# engine changing is worth a red run.
#
# ONE REGEX PER LINE and every one of them has to match. A single alternation
# would be the weaker check by exactly the defect above: two halves joined by |
# are satisfied by either half, so the leg that vanished would take the other
# one's match and pass.
if [ -n "$MUST_RE" ]; then
  while IFS= read -r must; do
    [ -n "$must" ] || continue
    if ! grep -qE "$must" "$TMPDIR/guest-run.log"; then
      echo "the guest did not log the expected value: /$must/" >&2
      echo "see $TMPDIR/guest-run.log" >&2
      exit 1
    fi
    echo "    value check: /$must/"
  done <<MUSTEOF
$MUST_RE
MUSTEOF
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
