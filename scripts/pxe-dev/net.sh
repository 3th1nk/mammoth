#!/usr/bin/env bash
# Provisioning-network fixture for local PXE verification (scripts/pxe-dev).
# Creates a Linux bridge + tap pair and starts dnsmasq as the SITE DHCP —
# address allocation ONLY. It must NOT set dhcp-boot/TFTP options: the whole
# point is to verify mammoth's proxyDHCP answers the PXE part while a real
# DHCP owns the address part.
#
# Root on a Linux host (or a VM) — NOT CI, NOT macOS. See README.md.
set -euo pipefail

BR=br-pxe
TAP=tap-pxe
SUBNET=192.168.77
case "${1:-up}" in
up)
    ip link add "$BR" type bridge
    ip addr add "$SUBNET.1/24" dev "$BR"
    ip link set "$BR" up
    ip tuntap add dev "$TAP" mode tap
    ip link set "$TAP" master "$BR"
    ip link set "$TAP" up
    # dnsmasq: site DHCP only. --port=0 disables its DNS. NO dhcp-boot.
    dnsmasq --interface="$BR" --bind-interfaces --port=0 \
        --dhcp-range="$SUBNET.100,$SUBNET.200,12h" \
        --dhcp-option=option:router,"$SUBNET.1" \
        --log-queries --log-dhcp --no-daemon &
    echo "bridge $BR ($SUBNET.1/24) + tap $TAP up; dnsmasq (site DHCP) running"
    ;;
down)
    pkill -f "dnsmasq.*$BR" || true
    ip link del "$BR" || true
    ip tuntap del dev "$TAP" mode tap || true
    echo "fixture down"
    ;;
*)
    echo "usage: $0 [up|down]" >&2
    exit 2
    ;;
esac
