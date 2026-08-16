#!/usr/bin/env bash
# Fail if any named package's statement coverage is below the threshold.
#
# Usage: coverage-gate.sh <min-percent> <pkg-path-fragment>...
#
# Packages that do not exist yet are skipped with a notice rather than failing —
# the gate is written at M0 but only bites once internal/{ir,analysis,luagen}
# carry real logic at M1. Once a package exists, it is enforced.
set -euo pipefail

min="$1"; shift
[ -f coverage.out ] || { echo "coverage-gate: coverage.out not found"; exit 1; }

fail=0
for pkg in "$@"; do
  if [ ! -d "$pkg" ] || [ -z "$(find "$pkg" -name '*.go' -print -quit)" ]; then
    echo "coverage-gate: $pkg has no Go files yet — skipping"
    continue
  fi

  pct="$(go tool cover -func=coverage.out \
         | grep "/${pkg}/" \
         | awk '{gsub(/%/,"",$NF); s+=$NF; n++} END {if (n) printf "%.1f", s/n; else print "0"}')"

  if awk "BEGIN{exit !($pct < $min)}"; then
    echo "::error::$pkg coverage ${pct}% is below ${min}%"
    fail=1
  else
    echo "coverage-gate: $pkg ${pct}% >= ${min}%"
  fi
done

exit $fail
