#!/usr/bin/env bash
# Boot a qemu guest onto the provisioning bridge so it PXE-chains through
# mammoth: site DHCP (dnsmasq) gives the address, mammoth's proxyDHCP gives
# the NBP, iPXE fetches the script over HTTP, the installer rides
# inst.repo/inst.ks. Root needed for /dev/net/tun — NOT CI. See README.md.
set -euo pipefail

TAP=tap-pxe
MAC=52:54:00:12:34:56
MEM=${MEM:-4G}
DISK=${DISK:-/tmp/pxe-dev-disk.qcow2}
FIRMWARE=${FIRMWARE:-bios}   # bios | uefi
OVMF=${OVMF:-/usr/share/OVMF/OVMF_CODE.fd}

[ -e "$DISK" ] || qemu-img create -f qcow2 "$DISK" 40G

NETDEV="tap,id=n0,ifname=$TAP,script=no,downscript=no"
if [ "$FIRMWARE" = uefi ]; then
    BIOS=(-bios "$OVMF")
else
    BIOS=()
fi

exec qemu-system-x86_64 \
    -machine accel=kvm:tcg -m "$MEM" -smp 2 \
    "${BIOS[@]}" \
    -netdev "$NETDEV" -device virtio-net-pci,netdev=n0,mac=$MAC \
    -drive file="$DISK",if=virtio,format=qcow2 \
    -display none -serial mon:stdio
