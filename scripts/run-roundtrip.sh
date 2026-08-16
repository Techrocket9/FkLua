#!/usr/bin/env bash
# Prove guest state survives a REAL Factorio save/load cycle.
#
# scripts/run-guest.sh shows the mod loads and runs, and the host-side suite
# proves the round trip under bin/lua52f with a genuinely fresh Lua state. What
# neither can do is make Factorio itself write a save and read it back, because
# `--benchmark` never saves -- `game.auto_save()` under it is silently a no-op,
# which was measured rather than assumed.
#
# A HEADLESS SERVER does honour it ("Only the server will save in multiplayer").
# So: start one, let a tiny trigger mod call game.auto_save at a known tick, wait
# for the file, kill the server, then benchmark the save that came out.
#
# The trigger is a separate mod on purpose. Nothing FkLua ships knows this test
# exists, and control.lua stays the file a mod author reads.
#
# The discriminator is that the game tick RESUMES from the save while a guest
# that lost its state restarts from zero. At save tick 60 the guest has seen 61
# ticks, so ten ticks later:
#
#     state survived   tick 70 seen=71
#     state lost       tick 70 seen=10
#
# Both are checked -- the second by running the same save against a
# --persist=none build, so a pass means the check can actually tell them apart.
#
# FOUR GUESTS, and each of the last three earns its place. `hello` allocates a
# few hundred bytes, so its heap never leaves the linear memory TinyGo started
# it with -- and two save/load bugs in a row hid behind exactly that. M10 found
# a memory.grow that was never written back in table mode; the audit found the
# same thing again in packed mode, with this script green both times. `grow`
# takes its heap past the initial pages and checks every byte it wrote is still
# there, which is what makes the growing case visible here rather than only
# under the oracle.
#
# `growbig` is the same guest at 5 MiB behind `-tags growbig`, and it earns its
# place the same way the others did -- by covering a shape the smaller one
# cannot reach. Linear memory is a vector of 2^19-word SHARDS, so a 5 MiB heap
# is three tables of which the last is PARTIAL, and a save has to carry all
# three and a load has to rebuild them. Table mode aliases the vector into
# storage; packed mode's restore has to CREATE the shards the saved size implies,
# because a load rebuilds the memory at the module's DECLARED size and the save
# has pages for more. Those are two different bugs and neither is reachable at
# 1 MiB, which is where the flat representation left this script.
#
# `gcsave` is built with the COLLECTOR on (--gc=collected), and what it adds is
# a second kind of state in linear memory that neither of the others has: the
# allocator's span table, mark bitmap, free-run lists and class cursors.
# agents/gc.md claims those are carried across a save for free, in table and
# packed alike, because every one of them is guest memory rather than a Lua
# structure beside it -- and "for free" is a claim about a design until
# something saves a heap that a collection has actually touched. It collects
# several times before the save and several times after it, and every retained
# block is checksummed, because a block reclaimed while live does not trap: it
# comes back ZERO and then somebody else's.
#
# SINCE STAGE C THE SAVE LANDS IN THE MIDDLE OF A COLLECTION, which is the case
# that did not exist before pacing: a stop-the-world collector is never half
# done. gcsave collects continuously, so it is saved at two different ticks and
# the run demands that BOTH phases were interrupted at least once across the
# matrix -- mid-MARK, where the write barrier has to be re-armed from
# `storage.fk_gc` and the lost dirty-page record recovered by a full re-scan, and
# mid-SWEEP, where a cursor and a set of free runs have to come back and the
# collection still has to reach idle. They resume through different code and
# fail differently: a mark that resumes with the barrier off loses writes, a
# sweep that never finishes leaves the one-shot on_tick registered forever.
#
# AND THEN THERE IS THE MIGRATE LEG, which is not one of the guests above and
# does not run in their loop, because it wants the opposite of what they want:
# every leg above loads a save its OWN build wrote, and this one loads a save
# written by a build that no longer exists. It is the last thing this script
# does; its reasoning is beside it, at the bottom.
set -euo pipefail

# THE BACKGROUNDED SERVER DIES WITH THE SCRIPT THAT STARTED IT.
#
# make_save backgrounds a headless Factorio and kills it inline, and there are
# about a dozen of those sub-60-second windows in a full run. A SIGTERM to this
# script during one of them -- a runner cancelling, a session tearing down --
# used to orphan a server that LOCKS the user directory, which is the state the
# operations notes describe as "dies at startup and reads as a broken gate" for
# every subsequent in-game run, until somebody finds the process by hand.
#
# THE EXIT TRAP DOES NOT `exit` AND THE SIGNAL TRAPS MUST, which is not
# symmetric and was measured rather than assumed. An exiting EXIT trap replaces
# this script's own status with the trap's, and what the run exits with is the
# whole result -- so that one only kills. But a TERM trap that does not exit
# SWALLOWS the signal: bash runs the handler and resumes the poll loop, so the
# script carries on for another sixty seconds against a server it has just
# killed, and reports "no autosave was written". Confirmed by driving this exact
# pattern with a stand-in child. So INT and TERM kill and then exit 130/143,
# which is what an untrapped signal would have produced anyway, and the EXIT
# trap's second kill is a harmless no-op.
#
# An interactive Ctrl-C already signalled the whole process group, so what this
# adds is the targeted-signal case: a runner cancelling, a session tearing down.
SERVER_PID=""
stop_server() { kill "$SERVER_PID" 2>/dev/null || true; }
trap stop_server EXIT
trap 'stop_server; exit 130' INT
trap 'stop_server; exit 143' TERM

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORIO="${FACTORIO_BIN:-$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio}"
# The INSTALLED engine, which is a different axis from the API pin. See the file.
. "$ROOT/scripts/lib-engine.sh"
USERDIR="${FACTORIO_USERDIR:-$HOME/Library/Application Support/factorio}"
SAVEDIR="$USERDIR/saves"
TMPDIR="$ROOT/testdata/tmp"
# gcsave-rs -- THE RUST COLLECTED LEG -- IS IN THE DEFAULT LIST since its save
# ticks were measured. Both persist modes, both phases:
#
#     seen=301, 32/32 intact across 2 collections, resumed MID-MARK
#     seen=301, 32/32 intact across 2 collections, resumed MID-SWEEP
#
# So a Rust guest's heap AND its collector's own bookkeeping -- span table, mark
# bitmap, free runs, class cursors, sweep cursor -- survive a real save and
# reload, and a save taken in EITHER phase is resumed rather than restarted.
#
# `retain` is the PERSISTENT HANDLE leg, and it is here rather than in the
# host-side suite because of what it needs: a real Factorio serializing a real
# LuaObject reference into a real save. fk_abi.lua has always documented that
# space as save-surviving -- persistent_table() and adopt() both existed -- and
# fk_mod.lua called NEITHER, so a retained handle was valid for the session and
# ERR_BAD_HANDLE on the next load. The first mod that ever called fk_retain found
# it (fklua-ports' inventory-sensor, F1); nothing in this repo had, because every
# guest here re-reads the world instead. It checks two handles still resolving,
# and that the slot a RELEASED handle freed is the one the next retain gets --
# which is adopt() having rebuilt the free list from the saved table rather than
# it having been lost with the session.
GUESTS="${GUESTS:-hello grow growbig retain gcsave gcsave-rs}"
SAVE_TICK="${SAVE_TICK:-60}"
# The gcsave leg is saved at each of these, so that a collection is interrupted
# in both of its phases. Deterministic: the guest is in lockstep, so tick N is
# the same phase on every run of the same build.
# THE TICKS ARE MEASURED, NOT GUESSED, and the guest prints what they are
# measured from: examples/gcsave logs a line at every PHASE CHANGE, so a run of
# this script with CHECK_TICK large enough shows the whole cadence:
#
#     phase 0  -> 1 cycles=0      mark
#     phase 39 -> 2 cycles=0      sweep
#     phase 46 -> 1 cycles=1      mark again
#     phase 87 -> 2 cycles=1
#     phase 92 -> 1 cycles=2
#
# So 60 lands mid-MARK and 42 lands mid-SWEEP, and CHECK_TICK has to be past 92
# for the "collections on both sides of the save" test to have two of them.
#
# THE CADENCE MOVED AGAIN WITH THE GROW PACING, by two to three ticks, and the
# mid-SWEEP tick moved with it: the sweep window was 41-47 and is 39-45, so the
# old constant of 45 became its last tick and started landing mid-MARK. Nothing
# about the collector changed -- fkgc's grow increment is capped now, so this
# guest's heap climbs in smaller steps and the pressure that triggers a
# collection crosses its threshold a couple of ticks earlier. 42 is chosen with
# margin on both sides rather than at an edge, because the next thing that
# shifts the cadence by one should not fail this leg.
#
# THE CADENCE MOVED AT SHARDING STAGE C and these constants moved with it. It
# was a collection every ~4 ticks, because the mark terminated as soon as a full
# re-scan pass completed -- and that pass was completing WITHOUT COVERING THE
# HEAP, which is the use-after-free stage C fixed. This guest is deliberately
# over its budget, so its mark cannot converge at all; what ends it now is the
# forward-progress escape, which by construction has to watch for a bounded
# number of steps before it can conclude anything. A collection is ~47 ticks.
GC_SAVE_TICKS="${GC_SAVE_TICKS:-60 42}"
# THE RUST LEG HAS ITS OWN CADENCE AND ITS OWN CONSTANTS, because the cadence is
# a property of the GUEST'S HEAP and the two languages do not build the same one.
#
# RE-MEASURED 2026-08-03, AND THE REASON IT MOVED IS THE POINT. It was `30 239`,
# taken when a Rust collection was 65 ticks and then 177 and then over 218. Those
# numbers were of a guest running at the DEFAULT 256 KiB threshold -- because
# `fkgc`'s `initialize()` is lazy on this side, reached by the first allocation,
# and it assigned the defaults unconditionally, so `gc::set_threshold(8 << 10)`
# in this guest's own `fk_on_init` was overwritten by the very next line's
# allocation. The guest asked for an aggressive threshold, said so in a comment,
# and never got it. Fixed 2026-08-03 (fklua-ports' AutoDeconstruct, finding 3);
# this leg's constants are the first thing downstream of that fix, and the shift
# is the fix being real.
#
# Measured the same way as before, off the same phase lines, one --benchmark run
# to tick 460 with the guest logging every phase change:
#
#     phase 0   -> 1 cycles=0     cycle 0: mark 0-40,    sweep 41-45
#     phase 41  -> 2 cycles=0
#     phase 46  -> 1 cycles=1     cycle 1: mark 46-116,  sweep 117-123
#     phase 117 -> 2 cycles=1
#     phase 124 -> 1 cycles=2     cycle 2: mark 124-164, sweep 165-171
#     phase 165 -> 2 cycles=2
#     phase 172 -> 1 cycles=3     cycle 3: mark 172-212, sweep 213-219
#     phase 213 -> 2 cycles=3
#     phase 220 -> 1 cycles=4     cycle 4: mark 220-260, sweep 261-266
#
# So a collection is ~47 ticks and the collections no longer get longer, which is
# what an honoured threshold buys: the guest collects on heap pressure rather
# than on the live set having grown enough to reach 256 KiB.
#
# 60 lands mid-MARK in cycle 1 with 56 ticks of margin either side; 120 lands
# mid-SWEEP in cycle 1's five-tick sweep window (117-123), with three ticks
# either side. Narrow is not flaky: the guest is in lockstep, so tick N is the
# same phase on every run of the same build. What a shift produces is the loud
# "no save landed mid-sweep" failure at the bottom of this script, which is the
# intended way to find out -- and which is exactly how the threshold fix
# announced itself here.
#
# RE-DERIVED TWICE NOW, and both times by this gate rather than by anyone looking
# for it -- which is the argument for the gate. The threshold latch moved them
# once; the root-scan RESERVATION moved them again, because a Rust guest's roots
# are ~420 granules and holding that back changes how many steps a mark takes.
# The second time the leg failed LOUDLY in the other direction first: terms=0 and
# phase stuck at 1 across 300 ticks, i.e. no collection at all, which is a
# regression and not a schedule shift. The measurement that separates the two is
# the guest's own per-tick line -- cycles= RISING says the collector works and
# only the phase moved; cycles=0 says it does not, and no constant here will fix
# that.
#
# To re-derive: run this script, then read the phase column out of the log --
#   grep -oE 'tick [0-9]+ .*cycles=[0-9]+.*phase=[0-9]+' testdata/tmp/rt-server.log
# -- and pick one tick reporting phase=1 and one reporting phase=2.
GC_SAVE_TICKS_RS="${GC_SAVE_TICKS_RS:-60 180}"
CHECK_TICK="${CHECK_TICK:-120}"
# AND THE RUST LEG NEEDS LONGER, and this is the constant that kept the leg
# opt-in for a milestone. It was 200, and cycles=2 is not reached until tick
# 242 -- so BOTH arms tripped the "only 1 collection ran" gate before the phase
# was ever looked at, and the run reported "no save landed mid-sweep" for a
# reason that had nothing to do with the save tick. 300 clears the second
# collection's end by 58 ticks.
#
# The underlying cause is measured and is not this guest's: a Rust guest's
# statics are larger, the root re-scan is charged against the step budget, and at
# this guest's deliberate 512 a step spends a few hundred granules on roots
# before it does anything else. See agents/gc.md, "The Rust collector, as built".
#
# The alternative was to raise the guest's budget until its cadence matched, and
# it was rejected: the guest is a MIRROR, and retuning it to hide a real property
# of the port would make every other number it reports incomparable.
CHECK_TICK_RS="${CHECK_TICK_RS:-300}"
AUTOSAVE="$SAVEDIR/_autosave-fkrt.zip"

[ -x "$FACTORIO" ] || { echo "factorio not found at: $FACTORIO" >&2; exit 1; }
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
case " $GUESTS " in
  *" gcsave-rs "*) command -v cargo >/dev/null ||
    { echo "cargo is not installed: https://rustup.rs" >&2; exit 1; } ;;
esac

# THE COLLECTOR IS THE SAME COLLECTOR AND THE GUESTS ARE MIRRORS, so every
# assertion below is reused unchanged: guest/rust/examples/gcsave logs the same
# lines guest/go/examples/gcsave logs, character for character, which is what
# lets one Rust leg inherit patterns the Go leg already proved discriminate.
is_rust() { case "$1" in *-rs) return 0 ;; *) return 1 ;; esac; }
# The collected legs, in either language.
is_gcsave() { case "$1" in gcsave|gcsave-rs) return 0 ;; *) return 1 ;; esac; }
gc_save_ticks() { is_rust "$1" && echo "$GC_SAVE_TICKS_RS" || echo "$GC_SAVE_TICKS"; }
check_tick() { is_rust "$1" && echo "$CHECK_TICK_RS" || echo "$CHECK_TICK_BASE"; }

mkdir -p "$TMPDIR"
# gcsave is the collected leg, so it is built with -gc=custom and packaged with
# --gc=collected. Everything else keeps the shipping default: the point of the
# other two legs is that they are unaffected.
gc_flag() { is_gcsave "$1" && echo custom || echo leaking; }

# growbig is examples/grow behind a build tag rather than a directory of its
# own: same guest, same discriminator, 5 MiB of heap instead of 1. It is the leg
# that carries a memory of THREE SHARDS, the last of them partial, through a
# real save and back -- coverage that did not exist while linear memory was one
# flat table, and that neither persist mode can reach at 1 MiB.
src_dir() { [ "$1" = growbig ] && echo grow || echo "$1"; }
tag_flag() { [ "$1" = growbig ] && echo "-tags=growbig" || echo ""; }

# THE WASM IS ALWAYS REBUILT, AND THE CACHE THAT USED TO BE HERE IS WHY
# agents/testing.md HAS A SECTION ABOUT CACHES.
#
# It was `if [ ! -f "$wasm" ]`, so the guest was reused across runs whatever had
# changed underneath it. During sharding stage C that cost FOUR WRONG
# CONCLUSIONS IN A ROW: each collector fix was re-run against the binary the
# FIRST invocation had built, produced a byte-identical failure, and was read as
# "that was not the cause". The tell -- identical numbers from three genuinely
# different code paths -- was there and was missed.
#
# The lesson was written down and the code was not changed; it was worked around
# by hand with `rm testdata/tmp/*.wasm`, which is a cache whose key is whether
# somebody remembered. A warm TinyGo build is under a second and this script
# already spends minutes inside Factorio, so there is nothing to trade: rebuild
# every time, and fail loudly if the compiler produced nothing.
build_guest() {
  local name="$1" wasm="$TMPDIR/$1.wasm" tags
  if is_rust "$name"; then
    # ONE FLAG AND NO SOURCE CHANGE: `fk` owns the single #[global_allocator]
    # site and --features fk/fkgc chooses what backs it, so there is no Rust
    # analogue of the -gc=custom/import pair the TinyGo branch below needs.
    #
    # A SEPARATE TARGET DIR, and the wasm is copied out of it, for the same
    # reason the rebuild below exists: cargo writes every arm of a crate to one
    # artifact path, so a shared directory hands the next reader whichever arm
    # was built last.
    echo "==> building the ${name%-rs} guest with Rust (--features fk/fkgc)"
    rm -f "$wasm"
    ( cd "$ROOT/guest/rust" && CARGO_TARGET_DIR="$TMPDIR/cargo-rt" \
        cargo build --release --target wasm32-unknown-unknown \
        -p "${name%-rs}" --features fk/fkgc )
    cp "$TMPDIR/cargo-rt/wasm32-unknown-unknown/release/${name%-rs}.wasm" "$wasm"
    [ -s "$wasm" ] || { echo "cargo produced no $wasm for guest $name" >&2; exit 1; }
    return
  fi
  tags="$(tag_flag "$name")"
  echo "==> building the $name guest with TinyGo (-gc=$(gc_flag "$name") $tags)"
  rm -f "$wasm"
  ( cd "$ROOT/guest/go" && tinygo build -target=wasm-unknown -scheduler=none -opt=2 \
      "-gc=$(gc_flag "$name")" ${tags:+"$tags"} -o "$wasm" "./examples/$(src_dir "$name")" )
  [ -s "$wasm" ] || { echo "tinygo produced no $wasm for guest $name" >&2; exit 1; }
}
go build -o "$ROOT/bin/fklua" "$ROOT/cmd/fklua"

# Server settings with autosave effectively disabled: the only save we want is
# the one the trigger mod asks for, at a tick we chose.
cat > "$TMPDIR/rt-server-settings.json" <<'JSON'
{ "name": "fk", "description": "", "visibility": { "public": false, "lan": false },
  "username": "", "password": "", "token": "",
  "require_user_verification": false, "max_upload_in_kilobytes_per_second": 0,
  "minimum_latency_in_ticks": 0, "ignore_player_limit_for_returning_players": false,
  "allow_commands": "true", "autosave_interval": 1000000, "autosave_slots": 5,
  "afk_autokick_interval": 0, "auto_pause": false, "only_admins_can_pause_the_game": true,
  "autosave_only_on_server": true, "non_blocking_saving": false }
JSON

# Build a mod directory holding the guest at one --persist mode plus the trigger.
#
# `api` and `trigger` exist for the MIGRATE leg and default to what every other
# caller already got: the packaging pin fklua would choose on its own, and the
# one save tick the loop is driving. The leg needs both because it packages ONE
# wasm twice -- the pin is what makes the two builds differ, and the second
# server starts from a save that is already past the first trigger.
build_mods() {
  local mode="$1" dir="$2" wasm="$3" gc="${4:-leaking}" api="${5:-}" trigger="${6:-$SAVE_TICK}"
  rm -rf "$dir"; mkdir -p "$dir/fk-savetrigger_0.1.0"
  # DERIVED, not written down: the harness mod has to declare the same engine
  # series the packaged guest does, or the loader refuses one of the two and the
  # leg fails for a reason that has nothing to do with persistence. Note the
  # heredoc is UNQUOTED for exactly this one substitution.
  cat > "$dir/fk-savetrigger_0.1.0/info.json" <<JSON
{"name":"fk-savetrigger","version":"0.1.0","title":"FkLua save trigger",
 "author":"FkLua","factorio_version":"$SERIES","dependencies":["base"]}
JSON
  cat > "$dir/fk-savetrigger_0.1.0/control.lua" <<LUA
-- Test scaffolding, not part of FkLua. Asks the server to save at one tick.
script.on_event(defines.events.on_tick, function(e)
  if e.tick == $trigger then game.auto_save("fkrt") end
end)
LUA
  # UNSET means "whatever fklua defaults to", which is the point: -opt=3 became
  # the default at M7 and the hardcoded 2 here went on passing without ever
  # packaging what a user gets. run-guest.sh already does it this way.
  #
  # THE MOD NAME AND VERSION ARE FIXED, AND FOR THE MIGRATE LEG THAT IS THE
  # WHOLE POINT rather than an inherited default: a rebuild keeps the version,
  # which is precisely why Factorio does not raise on_configuration_changed for
  # one and why the handling had to move to the first outermost dispatch.
  # Bumping the version here would exercise the OTHER path and quietly stop
  # testing the one that broke.
  "$ROOT/bin/fklua" mod "$wasm" ${OPT:+--opt=$OPT} --persist="$mode" --gc="$gc" \
    ${api:+--api="$api"} --factorio-version "$SERIES" \
    --name fk-hello --version 0.1.0 --author FkLua -o "$dir" >/dev/null
  cat > "$dir/mod-list.json" <<'JSON'
{"mods":[{"name":"base","enabled":true},{"name":"fk-hello","enabled":true},
         {"name":"fk-savetrigger","enabled":true}]}
JSON
}

# Run a headless server on an EXISTING save until the autosave appears, then stop
# it. --until-tick does NOT apply to --start-server, so the file is the signal.
#
# Split out of make_save for the migrate leg, which has to take a SECOND save
# from a game that started off the first one -- and which must not re-`--create`,
# because a fresh map would throw away the stale-stamped state the whole leg is
# about. The trap bookkeeping is subtle enough (see the header) that a second
# copy of it would be the thing that drifts.
serve_until_save() {
  local dir="$1" save="$2"
  rm -f "$AUTOSAVE"
  "$FACTORIO" "${CFGARG[@]}" --start-server "$save" --mod-directory "$dir" \
    --server-settings "$TMPDIR/rt-server-settings.json" \
    --disable-audio >"$TMPDIR/rt-server.log" 2>&1 &
  local pid=$! i=0
  SERVER_PID="$pid"                         # ...so the EXIT trap can reach it
  while [ ! -f "$AUTOSAVE" ] && [ $i -lt 120 ]; do sleep 0.5; i=$((i+1)); done
  sleep 1                                   # let "Saving finished" land
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  SERVER_PID=""                             # ...and stops after the inline kill
  [ -f "$AUTOSAVE" ] || { echo "no autosave was written; see $TMPDIR/rt-server.log" >&2
                          grep -iE "error|saving" "$TMPDIR/rt-server.log" | tail -20 >&2
                          exit 1; }
}

# A fresh map, then a save off it.
make_save() {
  local dir="$1" map="$2"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --create "$map" --map-gen-seed 999 \
    --disable-audio >"$TMPDIR/rt-create.log" 2>&1
  serve_until_save "$dir" "$map"
}

# What the guest said in fk_after_load, which is the first tick after the load
# and the only place the RESUMED collection can be observed: `phase=` is 1 if
# the save interrupted a mark, 2 if it interrupted a sweep, 0 if it missed one.
loaded_line() {
  local dir="$1"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$AUTOSAVE" --benchmark-ticks 30 \
    --benchmark-runs 1 --disable-audio 2>&1 \
    | sed -n "s/.*\(\[gcsave\] loaded:.*\)/\1/p" | head -1
}

# HOW LONG THE LOADED RUN IS, derived rather than fixed at 30.
#
# The loaded save resumes at SAVE_TICK and the report line this script reads is
# the one at CHECK_TICK, so a fixed tick count silently caps how far apart those
# two may be: with 30, a CHECK_TICK more than 30 ticks past the save produced no
# report line at all and every leg failed as "state did NOT survive", which reads
# like a persistence bug and is a benchmark that stopped early. Ten ticks of
# margin because the guests log every tenth.
load_ticks() { echo $((CHECK_TICK - SAVE_TICK + 10)); }

# The guest's whole report line at CHECK_TICK after loading the save.
line_after_load() {
  local dir="$1"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$AUTOSAVE" --benchmark-ticks "$(load_ticks)" \
    --benchmark-runs 1 --disable-audio 2>&1 \
    | sed -n "s/.*\(tick $CHECK_TICK seen=.*\)/\1/p" | head -1
}

# ...and just the counter, which is the discriminator both guests share.
seen_in() { echo "$1" | sed -n "s/tick $CHECK_TICK seen=\([0-9]*\).*/\1/p"; }

# The WHOLE benchmark output for a save, kept in a file.
#
# The helpers above pull one line out of a run and throw the rest away, which is
# right for a counter and wrong for the migrate leg: half of what that leg
# asserts is the ABSENCE of a line (the rebuild warning), and you cannot grep for
# something that is not there in output nobody kept.
bench_log() {
  local dir="$1" ticks="$2" save="$3" out="$4"
  "$FACTORIO" "${CFGARG[@]}" --mod-directory "$dir" --benchmark "$save" \
    --benchmark-ticks "$ticks" --benchmark-runs 1 --disable-audio >"$out" 2>&1 || true
}

# CHECK_TICK IS PER GUEST from here on, so the base is kept and the live value is
# reassigned in the loop -- `survived`, `load_ticks` and `seen_in` all read it.
CHECK_TICK_BASE="$CHECK_TICK"
fail=0
# The control compares against whatever save was written LAST, and since the
# gcsave leg saves at several ticks that is not necessarily $SAVE_TICK. Recorded
# in the loop rather than assumed, which is what the control was doing before
# the gcsave leg gained a second save tick and started reporting a false failure.
last_save_tick="$SAVE_TICK"

phases_seen=""

for guest in $GUESTS; do
  build_guest "$guest"
  CHECK_TICK="$(check_tick "$guest")"
  survived=$((CHECK_TICK + 1))   # counter carried across the save
  ticks="$SAVE_TICK"
  is_gcsave "$guest" && ticks="$(gc_save_ticks "$guest")"
  for mode in table packed; do
   for SAVE_TICK in $ticks; do
    echo "==> $guest --persist=$mode: save at tick $SAVE_TICK, load, check tick $CHECK_TICK"
    dir="$TMPDIR/rt-$guest-$mode"
    last_save_tick="$SAVE_TICK"
    gcmode=leaking; is_gcsave "$guest" && gcmode=collected
    build_mods "$mode" "$dir" "$TMPDIR/$guest.wasm" "$gcmode"
    make_save "$dir" "$dir.zip"
    line="$(line_after_load "$dir")"
    got="$(seen_in "$line")"
    if [ "$got" != "$survived" ]; then
      echo "    seen=${got:-<nothing>}, wanted $survived -- state did NOT survive" >&2
      [ -n "$line" ] || echo "    (no report line at all: the guest may have trapped)" >&2
      fail=1
      continue
    fi
    # For the growing guest the counter is not enough: it would keep counting
    # even if the pages it grew into came back empty. Every block it wrote has
    # to still hold what it wrote.
    #
    # growbig is the same assertion at a size that spans FOUR shards: `blocks=`
    # is what the guest itself reports, so the expected count comes from the
    # build tag rather than from a constant here that would silently stop
    # matching. A leg that checked only the counter would keep counting while
    # the shards past the first came back empty.
    if [ "$guest" = retain ]; then
      # live=2 is the promise: both handles retained before the save still name
      # their surfaces after it, checked by CALLING something on them rather
      # than by looking at the number.
      #
      # reused=11 is the half a partial fix gets wrong. The guest retained slots
      # 10, 11 and 12 and released 11 before the save, so a rebuilt free list
      # hands 11 back to the next retain. A load that adopted the table but
      # restored a STALE free list, or that adopted nothing at all, gives 13 (or
      # 10) here -- and the first of those is corruption rather than a leak, two
      # guest handles aliasing one object.
      case "$line" in
        *"live=2 reused=11"*)
          echo "    seen=$got, both retained handles resolved and the freed slot came back" ;;
        *)
          echo "    seen=$got but the persistent handle space did not survive: $line" >&2
          fail=1 ;;
      esac
    elif [ "$guest" = grow ] || [ "$guest" = growbig ]; then
      want=16
      [ "$guest" = growbig ] && want=10
      case "$line" in
        *"blocks=$want intact=$want"*)
          echo "    seen=$got, all $want grown blocks intact -- the grown heap survived" ;;
        *)
          echo "    seen=$got but the grown heap did not come back whole: $line" >&2
          fail=1 ;;
      esac
    elif is_gcsave "$guest"; then
      # Two things, and the second is what this leg is for. Every retained
      # block still holds what was written into it, AND collections happened on
      # BOTH sides of the save -- a run whose cycle count is still zero after
      # the load saved a heap no collector had touched and proved nothing.
      cycles="$(echo "$line" | sed -n 's/.*cycles=\([0-9]*\).*/\1/p')"
      loaded="$(loaded_line "$dir")"
      phase="$(echo "$loaded" | sed -n 's/.*phase=\([0-9]*\).*/\1/p')"
      case "$line" in
        *"blocks=32 intact=32"*)
          if [ "${cycles:-0}" -lt 2 ]; then
            echo "    seen=$got and blocks intact, but only ${cycles:-0} collection(s) ran: the save did not cross one" >&2
            fail=1
          else
            case "${phase:-0}" in
              1) phases_seen="$phases_seen $guest:mark"
                 echo "    seen=$got, 32/32 intact across $cycles collections, resumed MID-MARK" ;;
              2) phases_seen="$phases_seen $guest:sweep"
                 echo "    seen=$got, 32/32 intact across $cycles collections, resumed MID-SWEEP" ;;
              *) echo "    seen=$got, 32/32 intact across $cycles collections, but the save did not land inside a collection (phase=${phase:-<none>}): $loaded" >&2
                 fail=1 ;;
            esac
          fi ;;
        *)
          echo "    seen=$got but a retained block did not come back whole: $line" >&2
          fail=1 ;;
      esac
    else
      echo "    seen=$got -- state survived the save"
    fi
   done
  done
done

# BOTH PHASES, or the mid-collection leg proved only one of two things. A mark
# resumes with a barrier to re-arm and a lost page record to recover from; a
# sweep resumes with a cursor and a set of free runs. Nothing about one implies
# the other.
#
# PER GUEST, AND NOT ACROSS THE MATRIX. The two collected legs are two languages
# with two cadences, so a pooled check would pass on mark from one and sweep from
# the other -- and the leg that never interrupted a sweep would be the one nobody
# heard about. Each collected guest must have crossed both phases on its own.
for guest in $GUESTS; do
  is_gcsave "$guest" || continue
  for want in mark sweep; do
    case " $phases_seen " in
      *" $guest:$want "*) ;;
      *) echo "==> $guest: no save landed mid-$want (saw:${phases_seen:- nothing})." >&2
         echo "    Adjust $(is_rust "$guest" && echo GC_SAVE_TICKS_RS || echo GC_SAVE_TICKS): the guest is deterministic, so a tick that misses will always miss." >&2
         echo "    Its phase lines are in the log and are what the constants are measured from." >&2
         fail=1 ;;
    esac
  done
done

# The control. A build that persists nothing must restart from zero; without it
# a pass proves only that the numbers were not read. It runs against whatever
# save the loop wrote last, so it is built from hello either way -- a mod whose
# guest differs from the one in the save is a different build, which is the
# state the control wants.
echo "==> control: --persist=none against the same save"
lost=$((CHECK_TICK - last_save_tick))                  # counter restarted
build_mods none "$TMPDIR/rt-none" "$TMPDIR/hello.wasm"
got="$(seen_in "$(line_after_load "$TMPDIR/rt-none")")"
if [ "$got" = "$lost" ]; then
  echo "    seen=$got -- restarted from zero, so the check discriminates"
else
  echo "    seen=${got:-<nothing>}, wanted $lost -- the control did not behave as expected" >&2
  fail=1
fi

# THE MIGRATE LEG: a REBUILT guest, in a real game, for the first time.
#
# fk_migrate has existed since M6 and had never run inside Factorio. The
# host-side suite covers it thoroughly (rebuild_test.go models three sessions in
# one interpreter), and on 2026-08-07 the declined-adoption fix moved its trigger
# somewhere a host-side test can model and cannot reach: off
# script.on_configuration_changed, which Factorio raises for a mod-VERSION
# change, and onto the FIRST OUTERMOST DISPATCH after the load. A rebuild keeps
# the version -- that was the defect -- so the hook that used to fire is exactly
# the hook a rebuild does not raise, and what replaced it is a code path inside a
# running game.
#
# THE TWO BUILDS ARE ONE WASM PACKAGED AT TWO API PINS, which is the cheapest
# honest rebuild there is. The stamp folds the pin in (cmd/fklua's
# TestTheAPIPinIsPartOfTheBuildStamp asserts exactly this, and asserts the chunks
# differ in the stamp LINE and nothing else), so the guest bytes never move and
# the only thing the second build can notice is that the save was written by
# somebody else. Packaging the same wasm twice at one pin would produce an
# identical stamp and test nothing; changing the guest source would change two
# things at once.
#
# THE MAP IS DELIBERATELY CROSS-STAMP, which is the opposite of what every other
# leg wants, so nothing here may "helpfully" keep the two builds in step: the
# mod name and version are pinned in build_mods (see its header), the second
# server starts from the FIRST save rather than from --create, and the resave
# trigger is a later tick because the loaded game is already past the first one.
#
# Four assertions, and they fail in four different places:
#
#   told=7           fk_migrate ran AND was handed the old state version out of
#                    the save. 0 would be the hook firing with nothing behind it.
#   no warning       the runtime's "this mod was rebuilt ... Guest state has
#                    been reset" line is the arm for a guest exporting NEITHER
#                    hook. A guest that handled the rebuild must not see it, and
#                    its presence would mean the dispatch never happened.
#   sentinel=0       the heap really is FRESH. fk_migrate is the notification
#                    half; fk_migrate_adopt is the one that hands the old bytes
#                    over. Both restart the counter, so no assertion about seen=
#                    can tell them apart -- this is the one that can.
#   no second        ON A SECOND LOAD, which is what finish_rebuild's state_init
#   notification     buys: the stamp is republished, so a save taken afterwards
#                    is the new build's own and loads clean. Before the fix this
#                    was the real damage -- the save stayed self-inconsistent and
#                    EVERY later load reset guest state again, forever.
#
# THE LAST ONE IS THE ABSENCE OF A LOG LINE AND NOT A COUNTER, and that was
# measured rather than chosen. The guest's `migrated=` field lives in the guest
# HEAP, so a clean second load CARRIES it: the first attempt at this leg asserted
# migrated=0 and read migrated=1 on a load that had plainly not migrated again --
# `seen=` in the same line said the heap had been adopted whole. A field on the
# tick report cannot distinguish "fired again just now" from "fired once, a save
# ago", because every such field survives the save by construction. The log line
# is emitted by the hook and by nothing else.
#
# So the second load is checked three ways, and the pair is what makes it tight:
# the notification line is ABSENT, `migrated=1` is still there (the migrated
# heap was carried rather than replaced by another fresh one), and `seen=`
# resumed -- the same discriminator every other leg uses, applied to a save the
# migrate itself wrote.
RUN_MIGRATE="${RUN_MIGRATE:-1}"
# The other committed pin. Any version under api/ that is not the default works;
# what matters is only that it differs, because the stamp is a hash and not an
# ordering.
MIGRATE_OTHER_API="${MIGRATE_OTHER_API:-2.1.12}"
MIGRATE_SAVE_TICK="${MIGRATE_SAVE_TICK:-60}"
MIGRATE_RESAVE_TICK="${MIGRATE_RESAVE_TICK:-90}"
MIGRATE_CHECK_TICK="${MIGRATE_CHECK_TICK:-120}"

if [ "$RUN_MIGRATE" = 1 ]; then
  echo "==> migrate: one wasm at two API pins, so the second build inherits a stale save"
  build_guest migrate
  for mode in table packed; do
    dirA="$TMPDIR/rt-migrate-$mode-a"
    dirB="$TMPDIR/rt-migrate-$mode-b"
    logA="$TMPDIR/rt-migrate-$mode-stale.log"
    logB="$TMPDIR/rt-migrate-$mode-clean.log"

    # Build A at whatever fklua defaults to, and take a save with it.
    build_mods "$mode" "$dirA" "$TMPDIR/migrate.wasm" leaking "" "$MIGRATE_SAVE_TICK"
    make_save "$dirA" "$dirA.zip"
    cp "$AUTOSAVE" "$TMPDIR/rt-migrate-$mode-stale.zip"

    # Build B: the same bytes at the other pin, so same_build() is false.
    build_mods "$mode" "$dirB" "$TMPDIR/migrate.wasm" leaking "$MIGRATE_OTHER_API" \
      "$MIGRATE_RESAVE_TICK"
    stampA=$(sed -n 's/.*build = "\([^"]*\)".*/\1/p' "$dirA/fk-hello_0.1.0/fk_module.lua" | head -1)
    stampB=$(sed -n 's/.*build = "\([^"]*\)".*/\1/p' "$dirB/fk-hello_0.1.0/fk_module.lua" | head -1)
    if [ -z "$stampA" ] || [ "$stampA" = "$stampB" ]; then
      echo "    the two packages share a build stamp ($stampA); there is no rebuild" >&2
      echo "    to notice, so this leg would pass without exercising anything." >&2
      fail=1
      continue
    fi

    # Load the stale save with build B. finish_rebuild runs at the first
    # outermost dispatch, i.e. before the first tick's handler.
    bench_log "$dirB" "$((MIGRATE_CHECK_TICK - MIGRATE_SAVE_TICK + 10))" \
      "$TMPDIR/rt-migrate-$mode-stale.zip" "$logA"
    told="$(sed -n 's/.*migrate told=\([0-9]*\) sentinel=\([0-9]*\).*/\1/p' "$logA" | head -1)"
    sent="$(sed -n 's/.*migrate told=\([0-9]*\) sentinel=\([0-9]*\).*/\2/p' "$logA" | head -1)"
    if [ "$told" != 7 ]; then
      echo "    --persist=$mode: the guest was NOT told about the rebuild (told=${told:-<no line>})." >&2
      echo "    $stampA -> $stampB; fk_migrate should have been dispatched at the" >&2
      echo "    first outermost dispatch after the load. See $logA" >&2
      fail=1
      continue
    fi
    if [ "$sent" != 0 ]; then
      echo "    --persist=$mode: fk_migrate ran on a heap that was NOT fresh" >&2
      echo "    (sentinel=$sent, wanted 0). That is fk_migrate_adopt's behaviour," >&2
      echo "    and the rodata hazard is exactly why the two are separate hooks." >&2
      fail=1
    fi
    if grep -q "this mod was rebuilt" "$logA"; then
      echo "    --persist=$mode: the rebuild WARNING was logged even though the" >&2
      echo "    guest exports fk_migrate, so the dispatch did not take that arm:" >&2
      grep "this mod was rebuilt" "$logA" | head -1 >&2
      fail=1
    fi
    echo "    --persist=$mode: $stampA -> $stampB, told=$told on a fresh heap, no warning"

    # ...and the second load. A server started on the stale save with build B
    # republishes the stamp at its first dispatch, so the save it then writes is
    # B's own -- and loading THAT must be an ordinary adopt.
    serve_until_save "$dirB" "$TMPDIR/rt-migrate-$mode-stale.zip"
    cp "$AUTOSAVE" "$TMPDIR/rt-migrate-$mode-clean.zip"
    bench_log "$dirB" "$((MIGRATE_CHECK_TICK - MIGRATE_RESAVE_TICK + 10))" \
      "$TMPDIR/rt-migrate-$mode-clean.zip" "$logB"
    line="$(sed -n "s/.*\(tick $MIGRATE_CHECK_TICK seen=.*\)/\1/p" "$logB" | head -1)"
    carried="$(echo "$line" | sed -n 's/.*migrated=\([0-9]*\).*/\1/p')"
    seen2="$(echo "$line" | sed -n 's/tick [0-9]* seen=\([0-9]*\).*/\1/p')"
    # The counter has been running since the migrate reset it at
    # MIGRATE_SAVE_TICK, so an adopting load reads the whole span and a load
    # that reset again reads only the span since the resave.
    want_adopt=$((MIGRATE_CHECK_TICK - MIGRATE_SAVE_TICK))
    want_reset=$((MIGRATE_CHECK_TICK - MIGRATE_RESAVE_TICK))
    if [ -z "$line" ]; then
      echo "    --persist=$mode: no report line from the second load; see $logB" >&2
      fail=1
    elif grep -q "migrate told=" "$logB"; then
      echo "    --persist=$mode: THE SECOND LOAD MIGRATED AGAIN:" >&2
      grep "migrate told=" "$logB" | head -1 >&2
      echo "    finish_rebuild's state_init is what republishes the stamp, so a" >&2
      echo "    save taken after it must load clean. A save that keeps re-migrating" >&2
      echo "    resets guest state on every load, forever." >&2
      fail=1
    elif [ "$seen2" = "$want_reset" ]; then
      echo "    --persist=$mode: the second load restarted the counter (seen=$seen2)," >&2
      echo "    so nothing re-notified but the heap was not carried either: $line" >&2
      fail=1
    elif [ "$seen2" != "$want_adopt" ]; then
      echo "    --persist=$mode: seen=$seen2 on the second load, wanted $want_adopt" >&2
      echo "    (a reset would read $want_reset): $line" >&2
      fail=1
    elif [ "${carried:-x}" != 1 ]; then
      # The positive half. The heap that came back must be the MIGRATED one --
      # a second fresh heap plus a suppressed notification would read 0 here
      # and is a different, worse bug than either clause above.
      echo "    --persist=$mode: the heap came back but is not the migrated one" >&2
      echo "    (migrated=${carried:-<none>}, wanted 1): $line" >&2
      fail=1
    else
      echo "    --persist=$mode: the save the rebuild wrote loads clean -- no second" \
           "notification, migrated heap carried (migrated=1 seen=$seen2)"
    fi
  done
fi


rm -f "$AUTOSAVE"
[ "$fail" -eq 0 ] || { echo "==> FAILED" >&2; exit 1; }
echo "==> done"
