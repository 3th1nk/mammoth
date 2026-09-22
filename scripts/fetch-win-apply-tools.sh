#!/usr/bin/env bash
# Refresh the embedded windows apply-image toolchain (assets/win-apply/tools)
# from the Alpine 3.22 mirrors. Pinned versions — bump deliberately, re-run
# the boot spike, commit. See assets/win-apply/PROVENANCE.md.
set -euo pipefail
DIR=$(cd "$(dirname "$0")/.." && pwd)/assets/win-apply
MIRROR=${MIRROR:-https://dl-cdn.alpinelinux.org/alpine/v3.22}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pkgs=(
  "community/x86_64/wimlib-1.14.4-r0.apk"
  "community/x86_64/wimlib-libs-1.14.4-r0.apk"
  "main/x86_64/ntfs-3g-progs-2026.2.25-r0.apk"
  "main/x86_64/ntfs-3g-libs-2026.2.25-r0.apk"
  "main/x86_64/fuse3-libs-3.17.3-r0.apk"
  "main/x86_64/libuuid-2.41-r0.apk"
)

mkdir -p "$WORK/root" "$DIR/tools/usr/bin" "$DIR/tools/usr/sbin" "$DIR/tools/usr/lib"
for p in "${pkgs[@]}"; do
  echo "fetch $p"
  curl -sfLO "$MIRROR/$p"
  tar xzf "$(basename "$p")" -C "$WORK/root" --exclude=.SIGN.* --exclude=.PKGINFO --exclude=.pre-install* --exclude=.post-install* --exclude=.trigger*
done

cp "$WORK/root/usr/bin/wimlib-imagex"  "$DIR/tools/usr/bin/"
cp "$WORK/root/usr/sbin/mkntfs"        "$DIR/tools/usr/sbin/"
for lib in libwim.so.15 libntfs-3g.so.89 libfuse3.so.3 libuuid.so.1; do
  src=$(ls "$WORK"/root/usr/lib/$lib.* 2>/dev/null | head -1)
  [ -n "$src" ] || src="$WORK/root/usr/lib/$lib"
  cp "$src" "$DIR/tools/usr/lib/$lib"
done
chmod 0755 "$DIR/tools/usr/bin/"* "$DIR/tools/usr/sbin/"*
chmod 0644 "$DIR/tools/usr/lib/"*
echo "refreshed $(find "$DIR/tools" -type f | wc -l | tr -d ' ') files under assets/win-apply/tools"
