#!/usr/bin/env bash
# Every stage-A measurement, in one pass, on a quiet machine.
#
# THIS IS A MEASUREMENT HARNESS, NOT SHIPPING CODE.
#
# Run it with nothing else running: the differences it is looking for are at or
# below 1%, and agents/benchmarks.md's discipline is that a ratio inside the A/A
# interval is not a measurement. Every table it prints carries its own A/A cell
# for exactly that reason.
#
#   ./scratchpad/gc/run-all.sh 2>&1 | tee scratchpad/gc/RESULTS.txt
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
OUT=testdata/tmp/gc
mkdir -p "$OUT"

command -v tinygo >/dev/null || { echo "tinygo is not installed" >&2; exit 1; }
[ -x bin/lua52f ] || { echo "bin/lua52f is missing; run: make lua52f" >&2; exit 1; }
go build -o bin/fklua ./cmd/fklua

echo "=== building the guests ==="
( cd bench/guests/go && tinygo build -target=wasm-unknown -scheduler=none \
    -gc=leaking -opt=2 -o "$ROOT/$OUT/k-go.wasm" . )
( cd guest/go && tinygo build -target=wasm-unknown -scheduler=none \
    -gc=leaking -opt=2 -o "$ROOT/$OUT/churn.wasm" ./examples/churn )
for m in table packed; do
  s=t3; [ "$m" = packed ] && s=p3
  ./bin/fklua compile "$OUT/k-go.wasm"  --opt=3 --persist=$m -o "$OUT/k-go-$s.lua"    >/dev/null 2>&1
  ./bin/fklua compile "$OUT/churn.wasm" --opt=3 --persist=$m -o "$OUT/k-churn-$s.lua" >/dev/null 2>&1
done

echo
echo "=== 1. what the churn guest allocates, per event ==="
python3 - <<'PY'
import subprocess
src = open('testdata/tmp/gc/k-churn-t3.lua').read()
body = ("local imports = { env = { fk_log = function(p, n) end } }\n"
        "local M = (function(...)\n" + src + "\nend)(imports)\n"
        "M.exports['_initialize']()\n"
        "print('B/event over 200 :', M.exports['churn_bytes_per_event'](200))\n"
        "print('B/event over 2000:', M.exports['churn_bytes_per_event'](2000))\n"
        "local t0 = M.exports['churn_heap_top']()\n"
        "local c = M.exports['churn_events'](10000)\n"
        "local t1 = M.exports['churn_heap_top']()\n"
        "print('over 10000 events:', t1 - t0, 'bytes =', (t1 - t0) / 10000, 'B/event  checksum', c)\n")
open('testdata/tmp/gc/alloc.lua', 'w').write(body)
print(subprocess.run(['bin/lua52f', 'testdata/tmp/gc/alloc.lua'],
                     capture_output=True, text=True).stdout, end='')
PY

echo
echo "=== 2. where the armed page set's cost goes (mark calls vs stores) ==="
python3 scratchpad/gc/attribute.py

echo
echo "=== 3. barrier candidates on the real guest benchmarks ==="
python3 scratchpad/gc/bench.py --guest go --reps 2 --samples "${SAMPLES:-15}" \
  --json "$OUT/barrier-guests.json"

echo
echo "=== 4. barrier candidates on the allocation-churn guest ==="
python3 scratchpad/gc/bench.py --churn --reps 1 --samples "${CSAMPLES:-9}" \
  --json "$OUT/barrier-churn.json"

echo
echo "=== 5. barrier candidates on the bench --opt kernels ==="
SAMPLES="${OSAMPLES:-15}" python3 scratchpad/gc/optbench.py

echo
echo "=== 6. mark and sweep throughput ==="
WORDS=262144 REPS=8 python3 scratchpad/gc/markbench.py

echo
echo "=== done ==="
