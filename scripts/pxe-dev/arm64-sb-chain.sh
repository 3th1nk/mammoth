#!/bin/sh
# Arm64 Secure Boot chain — qemu verification (docs/09-roadmap.md, arm64 boot
# chain). AAVMF (aarch64 virtual firmware) HAS NO UEFI PXE STACK by upstream
# design — ArmVirtQemu.dsc: "NETWORK_SNP_ENABLE is IA32/X64/EBC only" — so the
# delivery leg (DHCP/TFTP) cannot be exercised on aarch64 guests at all; that
# leg is shared code already covered by unit tests and the x64 real-hardware
# chain. What CAN be verified here is the part unique to arm64: the signed
# binary pair actually boots an arm64 Secure Boot firmware. This harness
# proves exactly that: mammoth exports its external-kit assets, they are laid
# out on a FAT boot disk (shimaa64 as BOOTAA64.EFI, grubaa64 beside it), and
# an AAVMF guest with Secure Boot ON (Microsoft-cert VARS.ms) must load the
# shim, verify grub against the shim CA, and reach the GRUB banner.
#
#   shimaa64.efi (Microsoft-signed)  →  grubaa64.efi (Debian-signed)  → GRUB
#
# On real hardware the same two binaries arrive via DHCP/TFTP
# (client-arch 11 → shimaa64.efi) and the chain continues into the per-MAC
# HTTP grub config — the firmware-side logic verified here is identical.
#
# Usage: host$ scripts/pxe-dev/arm64-sb-chain.sh [--keep]
# Requires: docker, ~/mammoth-qxe/{AAVMF_CODE.secboot.fd, AAVMF_VARS.ms.fd}
# (Debian qemu-efi-aarch64).
set -e
REPO_PWD="$PWD"
cd "$(dirname "$0")/../.."

WORK=${PXE_ARM_WORK:-/tmp/pxe-arm64}
QXE=${PXE_ARM_QXE:-$HOME/mammoth-qxe}
AAVMF_CODE=${PXE_ARM_AAVMF:-$QXE/AAVMF_CODE.secboot.fd}
AAVMF_VARS_TPL=${PXE_ARM_VARS:-$QXE/AAVMF_VARS.ms.fd}
BRIDGE_IP=192.168.78.1
HTTP_PORT=18081

for f in "$AAVMF_CODE" "$AAVMF_VARS_TPL"; do
  [ -f "$f" ] || { echo "missing $f (Debian qemu-efi-aarch64 assets)"; exit 1; }
done

echo "· building mammoth + starting (external mode: exports the PXE kit)"
go build -o "$WORK/mammoth" ./cmd/mammoth
mkdir -p "$WORK/media" "$WORK/logs"
rm -rf "$WORK/media/netboot" 2>/dev/null || true

KEY=$(python3 -c "import base64,os; print(base64.b64encode(os.urandom(32)).decode())")
MAMMOTH_DATABASE_URL='postgres://mammoth:mammoth@localhost:55432/arm64_e2e?sslmode=disable' \
MAMMOTH_HTTP_ADDR=":$HTTP_PORT" MAMMOTH_API_TOKEN=devtoken MAMMOTH_MASTER_KEY="$KEY" \
MAMMOTH_MEDIA_DIR="$WORK/media" \
MAMMOTH_EXTERNAL_URL="http://$BRIDGE_IP:$HTTP_PORT" \
MAMMOTH_PXE_ENABLED=true MAMMOTH_PXE_MODE=external \
"$WORK/mammoth" serve --mode=all >"$WORK/logs/server.log" 2>&1 &
SERVER_PID=$!
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  docker rm -f pxe-arm-ctl >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
for i in $(seq 1 60); do curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null && break; sleep 0.5; done
curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null || { echo "mammoth not healthy"; tail -20 "$WORK/logs/server.log"; exit 1; }

KIT="$WORK/media/netboot/external-tftp"
ls "$KIT/shimaa64.efi" "$KIT/grubaa64.efi" "$KIT/grub/arm64-efi/command.lst" \
   "$KIT/grub/grub.cfg" >/dev/null && echo "  ✓ kit carries the arm64 chain ($KIT)"

echo "· container: FAT boot disk from the exported kit"
docker rm -f pxe-arm-ctl >/dev/null 2>&1 || true
docker run --rm -d --name pxe-arm-ctl --privileged \
  -v "$WORK:/work" -v "$AAVMF_CODE:/fw/code.fd:ro" -v "$AAVMF_VARS_TPL:/fw/vars-template.fd:ro" \
  alpine:3.22 sleep infinity >/dev/null
docker exec pxe-arm-ctl sh -c '
  apk add -q dosfstools mtools qemu-system-aarch64 qemu-img 2>&1 | tail -1
  dd if=/dev/zero of=/work/fat-sb.img bs=1M count=256 2>/dev/null
  mkfs.vfat -F 32 /work/fat-sb.img >/dev/null 2>&1
  mmd -i /work/fat-sb.img ::/EFI ::/EFI/BOOT
  # what the PXE/TFTP delivery would hand the firmware, laid out for disk boot
  mcopy -i /work/fat-sb.img /work/media/netboot/external-tftp/shimaa64.efi ::/EFI/BOOT/BOOTAA64.EFI
  mcopy -i /work/fat-sb.img /work/media/netboot/external-tftp/grubaa64.efi ::/EFI/BOOT/grubaa64.efi
  cp /fw/vars-template.fd /work/vars-sb.fd
  rm -f /work/logs/serial.log
  qemu-system-aarch64 -machine virt -cpu cortex-a57 -m 1024 -smp 2 -display none \
    -serial file:/work/logs/serial.log \
    -drive if=pflash,format=raw,readonly=on,file=/fw/code.fd \
    -drive if=pflash,format=raw,file=/work/vars-sb.fd \
    -drive file=/work/fat-sb.img,format=raw,if=none,id=d0 \
    -device virtio-blk-pci,drive=d0,disable-legacy=on \
    -daemonize -pidfile /work/qemu.pid
  echo "  ✓ guest booting (AAVMF secboot + VARS.ms: Secure Boot ON)"
'

echo "· waiting for the firmware to walk the shim → grub chain"
GRUB=""
for i in $(seq 1 60); do
  grep -aq "GNU GRUB" "$WORK/logs/serial.log" 2>/dev/null && { GRUB=1; break; }
  sleep 1
done
tail -8 "$WORK/logs/serial.log" | sed 's/^/  serial: /'
if [ -n "$GRUB" ]; then
  echo "ARM64 SECURE BOOT CHAIN ✓ — shim (Microsoft-signed) verified grub (Debian-signed) under Secure Boot; GRUB running"
else
  echo "ARM64 SECURE BOOT CHAIN ✗ (see $WORK/logs)"
  exit 1
fi
