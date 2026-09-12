#!/bin/sh
# Boot the probe ISO under qemu and wait for the report + poweroff.
# Local iteration loop for the ramdisk probe UEFI structure
# (docs/compat/huawei.md ramdisk retrospective).
#
# Usage: qemu-boot.sh bios|uefi <probe.iso> [report-file]
#
# The guest reaches the catch server (report-server.py) at 10.0.2.2 via
# qemu slirp; the probe ISO must have been built with the matching report
# URL (go test -tags probe_dev default: http://10.0.2.2:8765/...).
set -eu

MODE=${1:?usage: qemu-boot.sh bios|uefi <probe.iso> [report-file]}
ISO=${2:?usage: qemu-boot.sh bios|uefi <probe.iso> [report-file]}
REPORT=${3:-/tmp/probe-dev-report.json}
QEMU=${QEMU:-qemu-system-x86_64}
FW=${QEMU_FIRMWARE_DIR:-/opt/homebrew/share/qemu}

rm -f "$REPORT"

case "$MODE" in
bios)
  BIOS_ARGS=""
  ;;
uefi)
  # OVMF loads via pflash (code read-only + a private vars copy); qemu 11
  # rejects the same image through -bios ("could not load PC BIOS").
  VARS=$(mktemp /tmp/ovmf-vars.XXXXXX.fd)
  cp "$FW/edk2-i386-vars.fd" "$VARS"
  BIOS_ARGS="-drive if=pflash,format=raw,readonly=on,file=$FW/edk2-x86_64-code.fd -drive if=pflash,format=raw,file=$VARS"
  ;;
*)
  echo "mode must be bios or uefi" >&2
  exit 2
  ;;
esac

echo "[qemu-boot] mode=$MODE iso=$ISO report=$REPORT"
# Test disk: an MBR-partitioned raw image exercises the /sys scan path
# (sda + sda1) — created by scripts/probe-dev/make-testdisk.py.
# -no-reboot: a successful probe powers off -> qemu exits 0; a reboot loop
# (boot failure falling back to disk) also exits instead of spinning.
"$QEMU" -m 2048 $BIOS_ARGS -cdrom "$ISO" -boot d -no-reboot \
  -drive file=/tmp/probe-testdisk.raw,format=raw,if=ide \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  -nographic
RC=$?
[ -n "${VARS:-}" ] && rm -f "$VARS"

if [ -s "$REPORT" ]; then
  echo "[qemu-boot] SUCCESS ($MODE): report received"
  python3 -c "import json,sys; json.load(open('$REPORT')); print('[qemu-boot] report JSON valid')"
  exit 0
fi
echo "[qemu-boot] FAILED ($MODE): no report (qemu rc=$RC)"
exit 1
