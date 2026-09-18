#!/usr/bin/env bash
# Fetch (or upgrade) the Secure Boot network boot programs in assets/pxe from
# the Debian `shim-signed`, `grub-efi-amd64-signed`/`grub-efi-arm64-signed`
# and `grub-efi-{amd64,arm64}-bin` packages (the -bin packages carry the
# module-list tables grubnet fetches from its (tftp)/grub/ prefix). See
# assets/pxe/PROVENANCE.md for provenance and licensing rationale.
#
# Usage:
#   scripts/fetch-secureboot-bins.sh                 # fetch pinned versions, verify hashes
#   scripts/fetch-secureboot-bins.sh <shim> <grub>   # bump to new Debian package versions
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$REPO_ROOT/assets/pxe"
SHIM_POOL="https://deb.debian.org/debian/pool/main/s/shim-signed"
GRUB_SIGNED_POOL="https://deb.debian.org/debian/pool/main/g"
GRUB_BIN_POOL="https://deb.debian.org/debian/pool/main/g/grub2"

SHIM_PINNED="1.51~1+deb13u1+16.1-2~deb13u1"
SHIM_DEB_SHA_AMD64="3c802fa303c0e6bf126adee74028d1042e3120360d159c2c05181dc9b2f61005"
SHIM_DEB_SHA_ARM64="e228a68b298865e0f1b35f93dac8fc9644a47bde3487419efe305777e2d4a1f0"
GRUB_PINNED="1+2.12+9+deb13u2"
GRUB_DEB_SHA_AMD64="da8bb31308a3682a7d9343fc42bf7f603ad85bb5483eeb3ece23e2f1be2e9bbd"
GRUB_DEB_SHA_ARM64="488a67e0910789b701baaa9c5a6bb3091479df3d673ce01e35383773045e4eac"
GRUB_BIN_PINNED="2.12-9+deb13u2"

SHIM_VERSION="${1:-$SHIM_PINNED}"
GRUB_VERSION="${2:-$GRUB_PINNED}"
GRUB_BIN_VERSION="${GRUB_BIN_VERSION:-$GRUB_BIN_PINNED}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# extract one file from one .deb. macOS `ar` (BSD) has no --output, so fall
# back to the GNU style used in fetch-pxe-bins.sh.
extract() {
  local deb="$1" root="$2" path="$3"
  mkdir -p "$root"
  ar x "$deb" --output="$root" 2>/dev/null || (cd "$root" && ar x "$deb")
  tar -xJf "$root/data.tar.xz" -C "$root" "./$path"
}

download() { # <url> <file> <pinned-sha-or-empty>
  echo "==> downloading $1"
  curl -fL --retry 2 -o "$2" "$1"
  if [[ -n "${3:-}" ]]; then
    echo "$3  $2" | sha256sum -c -
  fi
}

# --- UEFI x64 ---
download "$SHIM_POOL/shim-signed_${SHIM_VERSION}_amd64.deb" "$TMP/shim-amd64.deb" \
  "$([[ "$SHIM_VERSION" == "$SHIM_PINNED" ]] && echo "$SHIM_DEB_SHA_AMD64")"
download "$GRUB_SIGNED_POOL/grub-efi-amd64-signed/grub-efi-amd64-signed_${GRUB_VERSION}_amd64.deb" "$TMP/grub-amd64.deb" \
  "$([[ "$GRUB_VERSION" == "$GRUB_PINNED" ]] && echo "$GRUB_DEB_SHA_AMD64")"

# --- UEFI aarch64 (same distro pairing rule: shim and GRUB from one distro) ---
download "$SHIM_POOL/shim-signed_${SHIM_VERSION}_arm64.deb" "$TMP/shim-arm64.deb" \
  "$([[ "$SHIM_VERSION" == "$SHIM_PINNED" ]] && echo "$SHIM_DEB_SHA_ARM64")"
download "$GRUB_SIGNED_POOL/grub-efi-arm64-signed/grub-efi-arm64-signed_${GRUB_VERSION}_arm64.deb" "$TMP/grub-arm64.deb" \
  "$([[ "$GRUB_VERSION" == "$GRUB_PINNED" ]] && echo "$GRUB_DEB_SHA_ARM64")"

# --- grubnet module tables (both arches, from the -bin packages) ---
download "$GRUB_BIN_POOL/grub-efi-amd64-bin_${GRUB_BIN_VERSION}_amd64.deb" "$TMP/grubbin-amd64.deb" ""
download "$GRUB_BIN_POOL/grub-efi-arm64-bin_${GRUB_BIN_VERSION}_arm64.deb" "$TMP/grubbin-arm64.deb" ""

echo "==> extracting"
# shim loads its second stage under the name grubx64.efi / grubaa64.efi (same
# directory); the network-capable grub binaries are grubnetx64/grubnetaa64
# .efi.signed, renamed here.
extract "$TMP/shim-amd64.deb" "$TMP/x-shim-amd64" usr/lib/shim/shimx64.efi.signed
extract "$TMP/grub-amd64.deb" "$TMP/x-grub-amd64" usr/lib/grub/x86_64-efi-signed/grubnetx64.efi.signed
extract "$TMP/shim-arm64.deb" "$TMP/x-shim-arm64" usr/lib/shim/shimaa64.efi.signed
extract "$TMP/grub-arm64.deb" "$TMP/x-grub-arm64" usr/lib/grub/arm64-efi-signed/grubnetaa64.efi.signed

cp "$TMP/x-shim-amd64/usr/lib/shim/shimx64.efi.signed" "$DEST/shimx64.efi"
cp "$TMP/x-grub-amd64/usr/lib/grub/x86_64-efi-signed/grubnetx64.efi.signed" "$DEST/grubx64.efi"
cp "$TMP/x-shim-arm64/usr/lib/shim/shimaa64.efi.signed" "$DEST/shimaa64.efi"
cp "$TMP/x-grub-arm64/usr/lib/grub/arm64-efi-signed/grubnetaa64.efi.signed" "$DEST/grubaa64.efi"

# Module tables: extract the whole .lst set per arch into grub/<arch>-efi/.
extract_lst() { # <deb> <src-subdir> <dest-dir>
  local deb="$1" sub="$2" dir="$3"
  local root="$TMP/lst-$sub"
  mkdir -p "$root" "$dir"
  ar x "$deb" --output="$root" 2>/dev/null || (cd "$root" && ar x "$deb")
  tar -xJf "$root/data.tar.xz" -C "$root" './usr/lib/grub/'"$sub"'/*.lst'
  cp "$root"/usr/lib/grub/"$sub"/*.lst "$dir"/
}
extract_lst "$TMP/grubbin-amd64.deb" x86_64-efi "$DEST/grub/x86_64-efi"
extract_lst "$TMP/grubbin-arm64.deb" arm64-efi "$DEST/grub/arm64-efi"

for f in shimx64.efi grubx64.efi shimaa64.efi grubaa64.efi; do
  echo "    $f  $(sha256sum "$DEST/$f" | cut -d' ' -f1)"
done

echo "==> done"
if [[ "$SHIM_VERSION" != "$SHIM_PINNED" || "$GRUB_VERSION" != "$GRUB_PINNED" || "$GRUB_BIN_VERSION" != "$GRUB_BIN_PINNED" ]]; then
  echo "NOTE: update assets/pxe/PROVENANCE.md (versions, .deb sha256: shim-amd64=$(sha256sum "$TMP/shim-amd64.deb" | cut -d' ' -f1) grub-amd64=$(sha256sum "$TMP/grub-amd64.deb" | cut -d' ' -f1) shim-arm64=$(sha256sum "$TMP/shim-arm64.deb" | cut -d' ' -f1) grub-arm64=$(sha256sum "$TMP/grub-arm64.deb" | cut -d' ' -f1) grubbin-amd64=$(sha256sum "$TMP/grubbin-amd64.deb" | cut -d' ' -f1) grubbin-arm64=$(sha256sum "$TMP/grubbin-arm64.deb" | cut -d' ' -f1)) and NOTICE if the license changed."
else
  echo "NOTE: hashes verified against PROVENANCE.md — nothing to update."
fi
