# Verifying a mod headlessly

Factorio runs headless, so "does it load, and does it do the same thing twice" is a scriptable check for any FkLua mod. This page is that recipe, generic over any mod.

Point `MOD` at what `fklua mod` wrote and `NAME` at your `[mod] name`. The `FACTORIO` path and the `config.ini` source below are the macOS Steam locations; adapt both to your install.

```sh
MOD=my-mod_0.1.0; NAME=my-mod
FACTORIO="$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio"

# A private write-data directory: Factorio locks its user directory, so a run
# sharing one with an open game dies at startup.
mkdir -p verify/mods verify/userdir/config
sed -e "s|^write-data=.*|write-data=$PWD/verify/userdir|" \
    "$HOME/Library/Application Support/factorio/config/config.ini" \
    > verify/userdir/config/config.ini

cp -R "$MOD" verify/mods/
echo "{\"mods\":[{\"name\":\"base\",\"enabled\":true},{\"name\":\"$NAME\",\"enabled\":true}]}" \
    > verify/mods/mod-list.json

# --create is where _initialize and fk_on_init run.
"$FACTORIO" -c verify/userdir/config/config.ini --mod-directory verify/mods \
    --create verify/map.zip --disable-audio > verify/create.log 2>&1

# --benchmark reloads that save and runs it, twice.
"$FACTORIO" -c verify/userdir/config/config.ini --mod-directory verify/mods \
    --benchmark verify/map.zip --benchmark-ticks 1200 --benchmark-runs 2 \
    --disable-audio > verify/run.log 2>&1

grep -E "Checksum for script|$NAME|[Ee]rror" verify/create.log
grep -E "$NAME|Performed|checksum:" verify/run.log
```

On the unmodified scaffold, that prints a script checksum, the `fk_on_init` line from the create log, the `fk_on_tick` lines from the run log, and the same `checksum:` twice; two runs disagreeing means a nondeterministic guest, which in a lockstep game is a desync.

Two limits:

- `--benchmark` never saves, so state that must survive a real save needs a headless server and `game.auto_save()`; [`scripts/run-roundtrip.sh`](../scripts/run-roundtrip.sh) is that shape.
- A headless `--create` has no player and no connected client, so any event that needs one never fires.

[`scripts/run-guest.sh`](../scripts/run-guest.sh) is this recipe wired to the FkLua repository's own example guests.

## Verifying it on a setting a player chose

Everything above runs your mod on the settings it declares, which is the load a player who never opens the settings screen gets. A player's chosen values reach a mod through `mod-settings.dat`, a binary file the engine writes and reads back at every load; `fklua modsettings` writes and reads it, so a headless check can run on chosen values too.

```sh
cat > chosen.json <<'JSON'
{"version": [2, 0, 77, 0],
 "startup": {"my-mod-hard-mode": true, "my-mod-stack-size": 40},
 "runtime-global": {},
 "runtime-per-user": {}}
JSON

fklua modsettings write --from chosen.json --out verify/mods/mod-settings.dat
```

The file goes in the mod directory beside the mod folders, not inside one, and the run above then loads with those values in place. `fklua modsettings read verify/mods/mod-settings.dat` after the run prints the same JSON shape back: the engine rewrites the file with the values it settled on, and it resets a number outside a setting's range and a dropdown value off its list to the default rather than keeping them, so reading the file back is what says which values the load actually used.

Startup settings reach the settings and data stages, which have a check of their own that never reaches `control.lua` and costs seconds rather than a map: [`docs/data-stage.md`](data-stage.md), "Verifying it".
