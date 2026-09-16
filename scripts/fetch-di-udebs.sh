#!/usr/bin/env bash
# Stage the netboot udeb archive subset for MAMMOTH_PXE_DI_UDEBS_DIR.
#
# The netinst ISO prunes the netboot-only installer components (its udeb
# index lists ~230 entries with no kernel-image-*-di at all), so a netboot
# install off the unpacked ISO pool dies in anna with "No kernel modules
# were found". Run this once per suite on the mammoth host (or any host that
# can reach a Debian archive mirror, then rsync the result over) and point
# MAMMOTH_PXE_DI_UDEBS_DIR at the output directory:
#
#   fetch-di-udebs.sh <output-dir> [suite] [mirror]
#
# The output shape mirrors what builder.StageNetbootPool consumes:
#   <out>/pool/**  (the archive's complete udeb set for the suite)
#   <out>/dists/<suite>/main/debian-installer/binary-amd64/Packages[.gz]
#
# This is a deployment-time staging step, NOT a runtime dependency: mammoth's
# install pools stay offline (docs/10 tech-stack D-decisions: the machine
# network never leaves the provisioning L2).
set -euo pipefail

OUT=${1:?usage: fetch-di-udebs.sh <output-dir> [suite] [mirror]}
SUITE=${2:-trixie}
MIRROR=${3:-https://mirrors.tuna.tsinghua.edu.cn/debian}
ARCH=amd64
IDX="dists/$SUITE/main/debian-installer/binary-$ARCH"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/Packages.gz" "$MIRROR/$IDX/Packages.gz"
gunzip -c "$TMP/Packages.gz" > "$TMP/Packages"

total=0; fetched=0; failed=0
while read -r rel; do
  total=$((total+1))
  dest="$OUT/$rel"
  [ -f "$dest" ] && continue
  mkdir -p "$(dirname "$dest")"
  if curl -fsSL -o "$dest" "$MIRROR/$rel"; then
    fetched=$((fetched+1))
  else
    echo "FETCH FAIL: $rel" >&2
    failed=$((failed+1))
  fi
done < <(grep '^Filename: ' "$TMP/Packages" | awk '{print $2}')

mkdir -p "$OUT/$IDX"
cp "$TMP/Packages.gz" "$OUT/$IDX/Packages.gz"
cp "$TMP/Packages" "$OUT/$IDX/Packages"

echo "staged $total udebs/debs ($fetched fetched, $failed failed) + complete index -> $OUT"
echo "set MAMMOTH_PXE_DI_UDEBS_DIR=$OUT"
[ "$failed" -eq 0 ]
