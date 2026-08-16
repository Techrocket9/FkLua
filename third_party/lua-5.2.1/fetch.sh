#!/usr/bin/env bash
# Fetch, patch and build lua52f — Lua 5.2.1 shaped like Factorio's sandbox.
#
# Only this script and patches/ are committed. The tarball and the extracted
# build/ tree are gitignored.
set -euo pipefail

VERSION="5.2.1"
TARBALL="lua-${VERSION}.tar.gz"
URL="https://www.lua.org/ftp/${TARBALL}"
# Canonical PUC-Rio release hash. Verified 2026-07-28.
SHA256="64304da87976133196f9e4c15250b70f444467b6ed80d7cfd7b3b982b5177be5"

cd "$(dirname "$0")"
HERE="$PWD"

if [ ! -f "$TARBALL" ]; then
  echo "==> fetching $URL"
  curl -fsSL -o "$TARBALL" "$URL"
fi

echo "==> verifying sha256"
if command -v sha256sum >/dev/null 2>&1; then
  echo "${SHA256}  ${TARBALL}" | sha256sum -c -
else
  echo "${SHA256}  ${TARBALL}" | shasum -a 256 -c -
fi

echo "==> extracting"
rm -rf build
mkdir build
tar xzf "$TARBALL" -C build --strip-components=1

echo "==> patching"
# strpack.c is an ordinary C file rather than a patch hunk so it stays readable
# and diffable against upstream 5.4.6. lstrlib.c #includes it; see
# patches/02-string-pack.patch.
cp "$HERE/strpack.c" build/src/strpack.c
for p in "$HERE"/patches/*.patch; do
  echo "    $(basename "$p")"
  patch -p1 -d build -s < "$p"
done

echo "==> building"
case "$(uname -s)" in
  Darwin) PLAT=macosx ;;
  Linux)  PLAT=linux  ;;
  *)      PLAT=posix  ;;
esac
# 'generic' avoids readline/ncurses, which we neither have nor want; the
# interpreter is only ever run non-interactively by the test harness.
make -C build "$PLAT" MYLIBS= MYCFLAGS="-DLUA_USE_POSIX -O2" -s 2>/dev/null \
  || make -C build generic -s

echo "==> built build/src/lua ($(build/src/lua -v 2>&1))"
