#!/usr/bin/env bash
# Fetch (or upgrade) the Secure Boot network boot programs in assets/pxe from
# the Debian `shim-signed` and `grub-efi-amd64-signed` packages. See
# assets/pxe/PROVENANCE.md for provenance and licensing rationale.
#
# Usage:
#   scripts/fetch-secureboot-bins.sh                 # fetch pinned versions, verify hashes
#   scripts/fetch-secureboot-bins.sh <shim> <grub>   # bump to new Debian package versions
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$REPO_ROOT/assets/pxe"
SHIM_POOL="https://deb.debian.org/debian/pool/main/s/shim-signed"
GRUB_POOL="https://deb.debian.org/debian/pool/main/g/grub-efi-amd64-signed"

SHIM_PINNED="1.51~1+deb13u1+16.1-2~deb13u1"
SHIM_DEB_SHA="3c802fa303c0e6bf126adee74028d1042e3120360d159c2c05181dc9b2f61005"
GRUB_PINNED="1+2.12+9+deb13u2"
GRUB_DEB_SHA="da8bb31308a3682a7d9343fc42bf7f603ad85bb5483eeb3ece23e2f1be2e9bbd"

SHIM_VERSION="${1:-$SHIM_PINNED}"
GRUB_VERSION="${2:-$GRUB_PINNED}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SHIM_DEB="shim-signed_${SHIM_VERSION}_amd64.deb"
GRUB_DEB="grub-efi-amd64-signed_${GRUB_VERSION}_amd64.deb"

echo "==> downloading $SHIM_POOL/$SHIM_DEB"
curl -fL --retry 2 -o "$TMP/$SHIM_DEB" "$SHIM_POOL/$SHIM_DEB"
echo "==> downloading $GRUB_POOL/$GRUB_DEB"
curl -fL --retry 2 -o "$TMP/$GRUB_DEB" "$GRUB_POOL/$GRUB_DEB"

# A pinned version must byte-match the recorded hash; a bumped version has no
# expectation yet — the script prints the new hashes for PROVENANCE.md.
if [[ "$SHIM_VERSION" == "$SHIM_PINNED" ]]; then
  echo "$SHIM_DEB_SHA  $TMP/$SHIM_DEB" | sha256sum -c -
fi
if [[ "$GRUB_VERSION" == "$GRUB_PINNED" ]]; then
  echo "$GRUB_DEB_SHA  $TMP/$GRUB_DEB" | sha256sum -c -
fi

# Extract one file from one .deb. macOS `ar` (BSD) has no --output, so fall
# back to the GNU style used in fetch-pxe-bins.sh.
extract() {
  local deb="$1" root="$2" path="$3"
  mkdir -p "$root"
  ar x "$deb" --output="$root" 2>/dev/null || (cd "$root" && ar x "$deb")
  tar -xJf "$root/data.tar.xz" -C "$root" "./$path"
}

echo "==> extracting"
extract "$TMP/$SHIM_DEB" "$TMP/shim" usr/lib/shim/shimx64.efi.signed
extract "$TMP/$GRUB_DEB" "$TMP/grub" usr/lib/grub/x86_64-efi-signed/grubnetx64.efi.signed

# shim loads its second stage under the name grubx64.efi (same directory); the
# network-capable grub binary is grubnetx64.efi.signed, renamed here.
cp "$TMP/shim/usr/lib/shim/shimx64.efi.signed" "$DEST/shimx64.efi"
cp "$TMP/grub/usr/lib/grub/x86_64-efi-signed/grubnetx64.efi.signed" "$DEST/grubx64.efi"

for f in shimx64.efi grubx64.efi; do
  echo "    $f  $(sha256sum "$DEST/$f" | cut -d' ' -f1)"
done

echo "==> done"
if [[ "$SHIM_VERSION" != "$SHIM_PINNED" || "$GRUB_VERSION" != "$GRUB_PINNED" ]]; then
  echo "NOTE: update assets/pxe/PROVENANCE.md (versions, .deb sha256: shim=$(sha256sum "$TMP/$SHIM_DEB" | cut -d' ' -f1) grub=$(sha256sum "$TMP/$GRUB_DEB" | cut -d' ' -f1)) and NOTICE if the license changed."
else
  echo "NOTE: hashes verified against PROVENANCE.md — nothing to update."
fi
