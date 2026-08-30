#!/usr/bin/env bash
set -euo pipefail
ROOT="$1"
FACTORIO="$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio"
USERDIR=/private/tmp/fk-r3probe
MODS="$USERDIR/mods"
rm -rf "$USERDIR"; mkdir -p "$USERDIR/config" "$MODS"
DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
if [ -f "$DEFAULT_CFG" ]; then
  sed "s|^write-data=.*|write-data=$USERDIR|" "$DEFAULT_CFG" > "$USERDIR/config/config.ini"
else
  printf '[path]\nread-data=__PATH__system-read-data__\nwrite-data=%s\n\n[general]\nlocale=auto\n' "$USERDIR" > "$USERDIR/config/config.ini"
fi
CFG=(-c "$USERDIR/config/config.ini")

"$ROOT/bin/fklua" mod /tmp/r3ctl.wasm --data-module /tmp/r3data.wasm \
  --name r3probe --version 0.1.0 --author FkLua \
  --factorio-version 2.0 -o "$MODS" >/dev/null
printf '{"mods":[{"name":"base","enabled":true},{"name":"r3probe","enabled":true}]}\n' > "$MODS/mod-list.json"

echo "### 1. --dump-data: does a mod-data prototype land, and does the simulation load?"
"$FACTORIO" "${CFG[@]}" --mod-directory "$MODS" --dump-data 2>&1 | tail -5
echo
echo "### 2. the dump's own answer"
python3 - "$USERDIR/script-output/data-raw-dump.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print("  mod-data present in data.raw:", "mod-data" in d)
md=d.get("mod-data",{})
print("  entries:", sorted(md.keys()))
if "r3probe-config" in md:
    e=md["r3probe-config"]
    print("  data_type:", e.get("data_type"))
    print("  data:", json.dumps(e.get("data"), sort_keys=True))
it=d.get("item",{}).get("r3probe-item")
print("  item present:", it is not None)
if it:
    sim=it.get("factoriopedia_simulation")
    print("  factoriopedia_simulation present:", sim is not None)
    if sim: print("  simulation:", json.dumps(sim, sort_keys=True))
PY
echo
echo "### 3. --create: does the CONTROL guest read the blob back?"
"$FACTORIO" "${CFG[@]}" --mod-directory "$MODS" --create "$USERDIR/r3.zip" >/dev/null 2>&1 || true
grep -E "R3PROBE|Error|error" "$USERDIR/factorio-current.log" | head -20
echo
echo "### 4. does anything headless RUN a simulation?"
grep -icE "simulation" "$USERDIR/factorio-current.log" || echo "  0 mentions of 'simulation' in the whole log"
