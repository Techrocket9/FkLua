# shellcheck shell=bash
#
# What the INSTALLED engine is, for the scripts that drive a real Factorio.
#
# THE TWO AXES, because everything in this file exists because they are not the
# same axis:
#
#   THE API PIN is a build-time fact -- which runtime-api.json the bindings and
#   the packaged member table came from. It lives in internal/factorio's
#   DefaultAPIVersion, in fklua.toml's `api`, and in --api=VERSION. It defaults
#   to the GENERAL-AVAILABILITY release, because a default is what a mod author
#   who has pinned nothing ships to players.
#
#   THE ENGINE is a run-time fact -- which Factorio is on this machine. Nothing
#   in the repo knows it; you have to ask the binary. It is what info.json's
#   factorio_version is a claim about, and it is what fkipc's version floor
#   gates on (helpers.game_version, read in game).
#
# They agree on a developer machine that happens to have GA installed, and they
# do not agree here: the pin is 2.0.x and the install is 2.1.14. A 2.1 engine
# REFUSES a mod whose info.json declares 2.0 -- "Incompatible Factorio version
# (current: 2.1, required: 2.0)", at game start, before a line of the mod runs
# -- so every in-game gate would report a broken gate for a mod that is fine.
# Hence factorio_series below, and hence every `fklua mod` in scripts/ passing
# --factorio-version "$(factorio_series)".
#
# ONE COPY, sourced, rather than the scrape repeated in eight scripts. Eight
# copies of a version derivation is this repo's own named failure shape -- two
# places that must agree about one fact, and nothing that notices when they
# stop.
#
# Every function here needs FACTORIO set to the binary. Source this file after
# the FACTORIO= line the scripts already have.

# factorio_version_triple prints the installed engine's full version: "2.1.14".
#
# `factorio --version` prints several "Version:" lines -- the build, then the
# save format, then the map input version -- so the first one is the only one
# that means what we want, and taking it by head -1 rather than by grep is
# deliberate: a later line matching the same pattern is exactly the trap.
factorio_version_triple() {
  local line
  line="$("$FACTORIO" --version 2>/dev/null | head -1)"
  # "Version: 2.1.14 (build 87180, mac-arm64, steam)"
  line="${line#Version: }"
  line="${line%% *}"
  case "$line" in
    [0-9]*.[0-9]*.[0-9]*) printf '%s\n' "$line" ;;
    *)
      echo "could not read a version out of \`$FACTORIO --version\`" >&2
      return 1
      ;;
  esac
}

# factorio_series prints the installed engine's major.minor: "2.1".
#
# info.json takes a SERIES and naming a patch release there makes the mod
# unloadable, which is the same rule internal/factorio's majorMinor follows.
factorio_series() {
  local v
  v="$(factorio_version_triple)" || return 1
  printf '%s\n' "${v%.*}"
}

# fkipc_min_engine prints the fkipc engine floor: "2.1.14".
#
# READ OUT OF THE LIBRARY rather than written here, because a floor spelled in
# two places is a floor that eventually disagrees with itself -- and this copy
# would be the one nobody recompiles. An empty result is a hard failure rather
# than a permissive default: the constant was respelled and this reader needs
# updating, which is a different thing from "the floor is zero".
fkipc_min_engine() {
  local v
  v="$(sed -n 's/^var MinEngineVersion = Version{ *\([0-9]*\), *\([0-9]*\), *\([0-9]*\) *}.*/\1.\2.\3/p' \
        "$ROOT/guest/go/fkipc/version.go")"
  if [ -z "$v" ]; then
    echo "could not read MinEngineVersion out of guest/go/fkipc/version.go" >&2
    echo "(the constant was renamed or respelled -- fix scripts/lib-engine.sh)" >&2
    return 1
  fi
  printf '%s\n' "$v"
}

# stamp_series DIR -- rewrite a hand-written harness mod's factorio_version to
# the installed engine's series, in place, in a COPY.
#
# The committed probe mods under testdata/ (fklua-probe, fklua-gctail,
# fklua-growprobe, fklua-shardprobe, fklua-ipcprobe) are bare Lua: no wasm, no
# FkLua runtime, no API pin, never packaged by `fklua mod`. So nothing in the
# pin revert touches them -- and they still hard-coded "2.1", which is the
# series they were authored and measured against and which is wrong on any
# other install.
#
# THE COMMITTED VALUE IS KEPT AND IS NOT LOAD-BEARING, which is the treatment
# rather than an omission. Kept, because the directory is a working mod somebody
# may drop into a mods folder by hand and a factorio_version of "" is not; not
# load-bearing, because every automated path copies it and stamps the installed
# series over it. To stop that becoming a value nobody reads and nobody
# maintains, the stamp SAYS SO when it changes something -- a one-line notice on
# a machine whose engine has moved away from the committed series, which is
# exactly when the committed value has started to rot.
stamp_series() {
  local dir="$1" info="$1/info.json" series had
  series="$(factorio_series)" || return 1
  [ -f "$info" ] || { echo "stamp_series: no info.json in $dir" >&2; return 1; }
  had="$(sed -n 's/.*"factorio_version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$info" | head -1)"
  if [ "$had" != "$series" ]; then
    echo "    info.json: factorio_version $had -> $series (the installed engine)"
  fi
  sed -i.bak "s/\"factorio_version\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"factorio_version\": \"$series\"/" \
    "$info"
  rm -f "$info.bak"
}

# version_lt A B -- true when A sorts strictly before B as a dotted triple.
version_lt() {
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -t. -k1,1n -k2,2n -k3,3n | head -1)" = "$1" ]
}

# require_fkipc_floor refuses, early and by name, on an engine below the floor.
#
# WHY A GATE REFUSES RATHER THAN RUNNING AND FAILING: below the floor fkipc is
# HARD-DISABLED -- the link is inert, no HELLO goes out, nothing is pumped -- so
# the session can never come up, and every leg of every IPC gate would sit at
# its deadline and then report a protocol failure. A timeout that means "this
# engine is too old" reads exactly like a timeout that means "the protocol is
# broken", which is the reported-green/reported-red version of the skip problem
# this repo has already been bitten by twice.
require_fkipc_floor() {
  local have floor
  have="$(factorio_version_triple)" || return 1
  floor="$(fkipc_min_engine)" || return 1
  if version_lt "$have" "$floor"; then
    cat >&2 <<EOF

fkipc is DISABLED on this engine, so this gate cannot pass.

  installed: Factorio $have
  required:  Factorio $floor or newer

Below the floor a headless recv_udp with a packet queued aborts the process in
C++ (TickClosure.cpp:91), which no pcall can catch, so the library refuses to
run at all: Open reports it, logs one line, and the mod stays inert. The gate
stops here rather than timing out leg by leg, which would look like a protocol
failure instead of an engine that is too old.

Point FACTORIO_BIN at a $floor+ install, or skip this gate.
EOF
    return 1
  fi
}
