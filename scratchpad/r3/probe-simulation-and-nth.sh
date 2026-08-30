#!/usr/bin/env bash
set -euo pipefail
ROOT="$1"
FACTORIO="$HOME/Library/Application Support/Steam/steamapps/common/Factorio/factorio.app/Contents/MacOS/factorio"
USERDIR=/private/tmp/fk-r3probe2
MODS="$USERDIR/mods"
rm -rf "$USERDIR"; mkdir -p "$USERDIR/config" "$MODS"
DEFAULT_CFG="$HOME/Library/Application Support/factorio/config/config.ini"
if [ -f "$DEFAULT_CFG" ]; then
  sed "s|^write-data=.*|write-data=$USERDIR|" "$DEFAULT_CFG" > "$USERDIR/config/config.ini"
else
  printf '[path]\nread-data=__PATH__system-read-data__\nwrite-data=%s\n\n[general]\nlocale=auto\n' "$USERDIR" > "$USERDIR/config/config.ini"
fi
CFG=(-c "$USERDIR/config/config.ini")

echo "### A. is a SIMULATION flag anywhere on the command line?"
"$FACTORIO" --help 2>&1 | grep -icE "simulation" || echo "  0 mentions of 'simulation' in --help"

echo
echo "### B. does the engine VALIDATE the init console command at load?"
"$ROOT/bin/fklua" mod /tmp/r3ctl.wasm --data-module /tmp/r3bad.wasm \
  --name r3bad --version 0.1.0 --author FkLua --factorio-version 2.0 -o "$MODS" >/dev/null
printf '{"mods":[{"name":"base","enabled":true},{"name":"r3bad","enabled":true}]}\n' > "$MODS/mod-list.json"
set +e
"$FACTORIO" "${CFG[@]}" --mod-directory "$MODS" --dump-data >"$USERDIR/bad.out" 2>&1
echo "  exit $?"
set -e
grep -iE "error|failed|Goodbye|checksum" "$USERDIR/bad.out" | tail -4
python3 - "$USERDIR/script-output/data-raw-dump.json" <<'PY'
import json,sys,os
if not os.path.exists(sys.argv[1]):
    print("  no dump written"); raise SystemExit
d=json.load(open(sys.argv[1]))
it=d.get("item",{}).get("r3bad-item")
print("  the mod with the BROKEN init loaded:", it is not None)
if it: print("  init as stored:", json.dumps(it.get("factoriopedia_simulation",{}).get("init")))
PY

echo
echo "### C. the PERIODIC HOOK in a real game"
rm -rf "$MODS"; mkdir -p "$MODS"
"$ROOT/bin/fklua" mod /tmp/r3nth.wasm --name r3nth --version 0.1.0 --author FkLua \
  --factorio-version 2.0 -o "$MODS" 2>&1 | grep -E "wired|wrote"
printf '{"mods":[{"name":"base","enabled":true},{"name":"r3nth","enabled":true}]}\n' > "$MODS/mod-list.json"
"$FACTORIO" "${CFG[@]}" --mod-directory "$MODS" --create "$USERDIR/nth.zip" >/dev/null 2>&1
"$FACTORIO" "${CFG[@]}" --mod-directory "$MODS" --benchmark "$USERDIR/nth.zip" \
  --benchmark-ticks 1200 --benchmark-runs 1 >/dev/null 2>&1
grep -E "R3NTH" "$USERDIR/factorio-current.log" | head -10
