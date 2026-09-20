#!/usr/bin/env bash
# Fetch (or upgrade) the wimboot program in assets/pxe from the upstream
# ipxe/wimboot GitHub release. wimboot has no Debian package — upstream
# release binaries are the distribution artifact. See assets/pxe/
# PROVENANCE.md ("wimboot") for provenance and licensing.
#
# Usage:
#   scripts/fetch-wimboot.sh           # fetch the pinned version, verify hash
#   scripts/fetch-wimboot.sh v2.10.0   # bump to a new upstream release tag
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$REPO_ROOT/assets/pxe"
PINNED="v2.9.0"
PINNED_SHA="5f067ccdc4d084d5bf77b6c853bd0f8402dfc2b4cd1b103d358993ae97fae8e3"

VERSION="${1:-$PINNED}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "==> downloading wimboot $VERSION"
# --http1.1: GitHub release downloads intermittently fail with HTTP/2
# framing errors (curl 16) that --retry alone will not revisit.
curl -fL --http1.1 --retry 3 --retry-all-errors -o "$TMP/wimboot" \
  "https://github.com/ipxe/wimboot/releases/download/${VERSION}/wimboot"

# A pinned version must byte-match the recorded hash; a bumped version has
# no expectation yet — the script prints the new hash for PROVENANCE.md.
if [[ "$VERSION" == "$PINNED" ]]; then
  echo "$PINNED_SHA  $TMP/wimboot" | shasum -a 256 -c -
fi

install -m 0644 "$TMP/wimboot" "$DEST/wimboot"
echo "==> installed $DEST/wimboot ($(wc -c < "$DEST/wimboot") bytes)"
if [[ "$VERSION" != "$PINNED" ]]; then
  echo "==> new sha256: $(shasum -a 256 "$DEST/wimboot" | cut -d' ' -f1)"
  echo "    update assets/pxe/PROVENANCE.md (wimboot section) accordingly"
fi
