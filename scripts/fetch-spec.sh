#!/usr/bin/env bash
# Fetch and convert the official WebAssembly spec testsuite.
#
# The converted .json + .wasm output is COMMITTED to testdata/spec, so CI needs
# neither network access nor a WABT install. Run this only when adding files to
# the corpus or bumping the pinned commit.
#
# Requires: wast2json (brew install wabt)
set -euo pipefail

# Pinned so the corpus cannot change under us. The testsuite mirror is
# auto-updated weekly, and a silently shifting corpus would make "pass rate must
# never fall" meaningless.
COMMIT="193e551ff22663995b1ac95dc62344133669e14b"   # 2026-06-17
BASE="https://raw.githubusercontent.com/WebAssembly/testsuite/${COMMIT}"

# The corpus grows one milestone at a time. Adding a file here is a deliberate
# act: it raises the bar the pass-rate gate holds us to.
FILES=(
  # M1 -- i32 arithmetic
  i32.wast

  # M2 -- control flow, calls, linear memory
  block.wast
  loop.wast
  if.wast
  br.wast
  br_if.wast
  br_table.wast
  return.wast
  nop.wast
  unreachable.wast
  select.wast
  call.wast
  call_indirect.wast
  local_get.wast
  local_set.wast
  local_tee.wast
  global.wast
  memory.wast
  address.wast
  endianness.wast
  memory_redundancy.wast
  memory_trap.wast
  forward.wast
  func.wast
  labels.wast
  switch.wast
  stack.wast
  fac.wast

  # M3a -- floating point
  f32.wast
  f32_cmp.wast
  f32_bitwise.wast
  f64.wast
  f64_cmp.wast
  f64_bitwise.wast
  float_literals.wast
  float_memory.wast
  float_misc.wast
  conversions.wast
  left-to-right.wast

  # M3b -- 64-bit integers
  i64.wast
  int_exprs.wast
  int_literals.wast
  traps.wast

  # M3c -- audit of blind spots. Everything below tests behaviour we already
  # claim to implement but were not exercising at all.
  start.wast
  load.wast
  store.wast
  align.wast
  const.wast
  data.wast
  elem.wast
  local_init.wast
  func_ptrs.wast
  memory_size.wast
  memory_grow.wast
  address0.wast
  address1.wast
  float_exprs0.wast
  float_exprs1.wast
  float_memory0.wast
  memory_trap0.wast
  memory_trap1.wast
  traps0.wast
  unreached-valid.wast
  unreached-invalid.wast
  binary-leb128.wast
  names.wast
  comments.wast
  token.wast
  id.wast
  type.wast
  custom.wast
  inline-module.wast
  obsolete-keywords.wast
  skip-stack-guard-page.wast
)

# The feature surface is TinyGo's -target=wasm-unknown, NOT pure wasm 1.0.
#
# TinyGo emits sign-extension and non-trapping float->int unconditionally on
# that target -- there is no flag to turn them off -- so a pure-MVP corpus would
# test a dialect none of our guests actually produce. Its exact feature string is
#   +nontrapping-fptoint,+sign-ext,-bulk-memory,-multivalue,-reference-types
# and these flags mirror it.
#
# (Copying wazero's v1 flag set verbatim fails immediately: today's i32.wast uses
# i32.extend8_s, which pure MVP rejects.)
#
# Later milestones relax this: M10 adds bulk-memory for TinyGo wasip1.
WAST2JSON_FLAGS=(
  --disable-simd
  --disable-multi-value
  --disable-bulk-memory
  --disable-reference-types
  --debug-names
)

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/testdata/spec"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

command -v wast2json >/dev/null || {
  echo "wast2json not found. Install with: brew install wabt" >&2; exit 1; }

echo "==> testsuite @ ${COMMIT:0:12}"
mkdir -p "$DEST"

# Upstream has moved to Wasm 3.0, where multi-value and reference types are
# core. Files leaning on those convert only partially, or not at all, under our
# feature flags -- which is correct, since no guest we target emits them. A
# conversion failure is therefore expected rather than an error, but it IS
# recorded: a corpus that silently shrinks would quietly weaken the pass gate.
CONVERTED=()
PARTIAL=()
FAILED=()

for f in "${FILES[@]}"; do
  name="${f%.wast}"
  printf '==> %-22s ' "$f"
  if ! curl -fsSL -o "$WORK/$f" "$BASE/$f"; then
    echo "FETCH FAILED"
    FAILED+=("$name (fetch)")
    continue
  fi

  out="$DEST/$name"
  rm -rf "$out"
  mkdir -p "$out"

  errs=0
  wast2json "${WAST2JSON_FLAGS[@]}" "$WORK/$f" -o "$out/$name.json" \
      2>"$WORK/$name.err" || errs=1

  if [ ! -f "$out/$name.json" ]; then
    echo "SKIPPED (needs features outside our surface)"
    rm -rf "$out"
    FAILED+=("$name")
    continue
  fi

  cmds=$(python3 -c "
import json
from collections import Counter
d=json.load(open('$out/$name.json'))
c=Counter(x['type'] for x in d['commands'])
print(' '.join(f'{k}={v}' for k,v in sorted(c.items())))
")
  if [ "$errs" = "1" ]; then
    n=$(grep -c "error:" "$WORK/$name.err" || true)
    echo "partial, $n module(s) skipped: $cmds"
    PARTIAL+=("$name ($n skipped)")
  else
    echo "$cmds"
    CONVERTED+=("$name")
  fi
done

{
  echo "WebAssembly/testsuite @ $COMMIT"
  echo "converted with wast2json $(wast2json --version) using:"
  echo "  ${WAST2JSON_FLAGS[*]}"
  echo
  echo "Regenerate with scripts/fetch-spec.sh. The output is committed on purpose"
  echo "so CI needs no network and no WABT."
  echo
  echo "Upstream is now Wasm 3.0, where multi-value and reference types are core."
  echo "Neither is in the surface our guests emit (TinyGo wasm-unknown is"
  echo "-multivalue -reference-types), so files depending on them convert only"
  echo "partially, or not at all. Recorded here rather than left implicit."
  echo
  echo "fully converted (${#CONVERTED[@]}):"
  for n in "${CONVERTED[@]}"; do echo "  $n"; done
  echo
  echo "partially converted (${#PARTIAL[@]}) -- some modules need excluded features:"
  for n in "${PARTIAL[@]}"; do echo "  $n"; done
  echo
  echo "not converted (${#FAILED[@]}):"
  for n in "${FAILED[@]}"; do echo "  $n"; done
} > "$DEST/SOURCE"

echo
echo "==> $DEST: ${#CONVERTED[@]} full, ${#PARTIAL[@]} partial, ${#FAILED[@]} skipped"
