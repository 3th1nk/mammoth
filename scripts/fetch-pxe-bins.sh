#!/usr/bin/env bash
# Fetch (or upgrade) the iPXE network boot programs in assets/pxe from the
# Debian `ipxe` package. See assets/pxe/PROVENANCE.md for provenance and
# licensing rationale.
#
# Usage:
#   scripts/fetch-pxe-bins.sh            # fetch the pinned version, verify hashes
#   scripts/fetch-pxe-bins.sh <version>  # bump to a new Debian package version
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$REPO_ROOT/assets/pxe"
POOL="https://deb.debian.org/debian/pool/main/i/ipxe"
PINNED="2.0.0+dfsg-5"
PINNED_DEB_SHA="f0f2b3df949f3207e2ca5c1324d23d0ad94a968c3d53e18ed8ea57b12f24a4bb"

VERSION="${1:-$PINNED}"
DEB="ipxe_${VERSION}_all.deb"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> downloading $POOL/$DEB"
curl -fL --retry 2 -o "$TMP/$DEB" "$POOL/$DEB"

# A pinned version must byte-match the recorded hash; a bumped version has
# no expectation yet — the script prints the new hash for PROVENANCE.md.
if [[ "$VERSION" == "$PINNED" ]]; then
  echo "$PINNED_DEB_SHA  $TMP/$DEB" | sha256sum -c -
fi

echo "==> extracting"
mkdir -p "$TMP/root"
ar x "$TMP/$DEB" --output="$TMP" 2>/dev/null || (cd "$TMP" && ar x "$DEB")
tar -xJf "$TMP/data.tar.xz" -C "$TMP/root" \
  ./usr/lib/ipxe/undionly.kpxe \
  ./usr/lib/ipxe/ipxe-amd64.efi \
  ./usr/lib/ipxe/ipxe-arm64.efi

for f in undionly.kpxe ipxe-amd64.efi ipxe-arm64.efi; do
  cp "$TMP/root/usr/lib/ipxe/$f" "$DEST/$f"
  echo "    $f  $(sha256sum "$DEST/$f" | cut -d' ' -f1)"
done

echo "==> done"
if [[ "$VERSION" != "$PINNED" ]]; then
  echo "NOTE: update assets/pxe/PROVENANCE.md (version, .deb sha256: $(sha256sum "$TMP/$DEB" | cut -d' ' -f1)) and NOTICE if the license changed."
else
  echo "NOTE: hashes verified against PROVENANCE.md — nothing to update."
fi
