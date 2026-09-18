#!/bin/sh
# External DHCP+TFTP escape hatch — same-shape verification (docs/operations.md
# §pxe-external): a privileged alpine container owns a bridge (br-pxe) with the
# site-PXE stand-ins — dnsmasq as the real DHCP+TFTP serving mammoth's exported
# static kit, socat bridging guest HTTP to the host's mammoth, and a TCG qemu
# UEFI guest on a tap. The host runs mammoth in MAMMOTH_PXE_MODE=external (no
# UDP binds). The guest then PXE-installs alpine through the agent path —
# proving the full external chain: DHCP → TFTP (shim→grubnet→trampoline) →
# HTTP per-MAC grub config → kernel/initrd → agent → callback.
#
# Usage: host$ scripts/pxe-dev/external-e2e.sh [--keep]
# Requires: docker, ~/mammoth-qxe/{alpine-extended,alpine-netboot tarball,OVMF}.
set -e
REPO_PWD="$PWD"
cd "$(dirname "$0")/../.."

WORK=${PXE_EXT_WORK:-/tmp/pxe-external}
QXE=${PXE_EXT_QXE:-$HOME/mammoth-qxe}
ALPINE_ISO=${PXE_EXT_ISO:-$QXE/alpine-extended-3.22.2-x86_64.iso}
NETBOOT=${PXE_EXT_NETBOOT:-$QXE/alpine-netboot-3.22.2-x86_64.tar.gz}
GUEST_MAC=52:54:00:12:34:56
BRIDGE_NET=192.168.77.0
BRIDGE_IP=192.168.77.1
HTTP_PORT=18080
KEEP=$1

for f in "$ALPINE_ISO" "$NETBOOT"; do
  [ -f "$f" ] || { echo "missing $f"; exit 1; }
done

echo "· building mammoth + starting (external mode)"
go build -o "$WORK/mammoth" ./cmd/mammoth
mkdir -p "$WORK/media" "$WORK/logs"
rm -rf "$WORK/media/netboot" 2>/dev/null || true

KEY=$(python3 -c "import base64,os; print(base64.b64encode(os.urandom(32)).decode())")
MAMMOTH_DATABASE_URL='postgres://mammoth:mammoth@localhost:55432/agent_e2e?sslmode=disable' \
MAMMOTH_HTTP_ADDR=":$HTTP_PORT" MAMMOTH_API_TOKEN=devtoken MAMMOTH_MASTER_KEY="$KEY" \
MAMMOTH_MEDIA_DIR="$WORK/media" \
MAMMOTH_EXTERNAL_URL="http://$BRIDGE_IP:$HTTP_PORT" \
MAMMOTH_PXE_ENABLED=true MAMMOTH_PXE_MODE=external \
MAMMOTH_PROBE_ALPINE_NETBOOT="file://$NETBOOT" \
MAMMOTH_PROBE_ALPINE_ISO="file://$ALPINE_ISO" \
"$WORK/mammoth" serve --mode=all >"$WORK/logs/server.log" 2>&1 &
SERVER_PID=$!
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  docker rm -f pxe-ext-ctl >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
for i in $(seq 1 60); do curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null && break; sleep 0.5; done
curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null || { echo "mammoth not healthy"; tail -20 "$WORK/logs/server.log"; exit 1; }
grep -q "external: DHCP/TFTP owned by the site" "$WORK/logs/server.log" && echo "  ✓ netboot in external mode (no UDP binds)"
KIT="$WORK/media/netboot/external-tftp"
ls "$KIT/undionly.kpxe" "$KIT/shimx64.efi" "$KIT/grub/grub.cfg" "$KIT/dnsmasq.conf.example" >/dev/null && echo "  ✓ external kit exported ($KIT)"

echo "· register machine + submit alpine install (PXE, agent path)"
CRED=$(curl -s -X POST -H "Authorization: Bearer devtoken" -H "Content-Type: application/json" "http://127.0.0.1:$HTTP_PORT/api/v1/credentials" -d '{"type":"bmc","name":"pxe-ext","secret":{"username":"a","password":"b"}}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
MID=$(curl -s -X POST -H "Authorization: Bearer devtoken" -H "Content-Type: application/json" "http://127.0.0.1:$HTTP_PORT/api/v1/machines" -d "{\"bmc\":{\"address\":\"fake://pxe-ext-$RANDOM\",\"protocol\":\"fake\",\"credential_id\":\"$CRED\"}}" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
# discovery must land BEFORE the install submission — verify_layout on an
# undiscovered machine is an instant LAYOUT_DISK_NOT_FOUND
for i in $(seq 1 30); do
  ST=$(curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/machines/$MID" | python3 -c "import json,sys; print(json.load(sys.stdin)['state'])" 2>/dev/null)
  [ "$ST" = "ready" ] && break; sleep 2
done
curl -s -X POST -H "Authorization: Bearer devtoken" -H "Content-Type: application/json" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs" -d '{
  "type": "install",
  "targets": {"machine_ids": ["'"$MID"'"]},
  "spec": {
    "boot": {"strategy": "pxe"},
    "image": {"source": "file://'"$ALPINE_ISO"'", "distro": "alpine"},
    "storage": {"disks": [{"select": {"match": {"type": "nvme", "size": "largest"}}, "wipe": true,
      "partitions": [
        {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
        {"size": "512M", "fs": "swap"},
        {"size": "rest", "fs": "ext4", "mount": "/"}]}]},
    "network": [{"match": {"mac": "'"$GUEST_MAC"'"}}, {"match": {"mac": "aa:bb:cc:dd:ee:01"}}],
    "identity": {"hostname_pattern": "pxe-ext-{index}"},
    "access": {"ssh_keys": []}
  }}' | python3 -c "import json,sys; d=json.load(sys.stdin); print('  ✓ job', d['id']) if 'id' in d else print('  ✗', d)"

# wait for prepare_media: the per-task boot tree (kernel/initrd/modloop/apks)
# must exist and the netboot entry armed for the guest MAC before the guest boots
for i in $(seq 1 120); do
  if curl -sf -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs" | grep -q '"type":"install"'; then break; fi
  sleep 1
done
TOKEN=""
for i in $(seq 1 300); do
  TOKEN=$(curl -s "http://127.0.0.1:$HTTP_PORT/netboot/script?mac=$GUEST_MAC" | grep -o 'files/[a-f0-9]*/' | head -1 | cut -d/ -f2)
  [ -n "$TOKEN" ] && break
  sleep 1
done
[ -n "$TOKEN" ] || { echo "✗ boot entry never armed"; tail -20 "$WORK/logs/server.log"; exit 1; }
echo "  ✓ netboot entry armed (token $TOKEN); script endpoint:"
curl -s "http://127.0.0.1:$HTTP_PORT/netboot/script?mac=$GUEST_MAC" | head -3 | sed 's/^/    /'
echo "  ✓ grub endpoint: $(curl -s "http://127.0.0.1:$HTTP_PORT/netboot/grub/$GUEST_MAC" | head -2 | tail -1)"

echo "· container: bridge + dnsmasq + qemu guest"
# OVMF must carry a NETWORK STACK — homebrew qemu's edk2 fd has none (the
# guest never PXEs). The Debian ovmf deb (a plain tar) ships the full 4M
# build; secboot variant keeps the shim chain honest under Secure Boot.
OVMF_DIR=/tmp/pxe-external/ovmf
if [ ! -f "$OVMF_DIR/code.fd" ]; then
  echo "· fetching Debian OVMF (network stack)"
  mkdir -p "$OVMF_DIR-x" && cd "$OVMF_DIR-x"
  curl -sL -o ovmf.deb https://deb.debian.org/debian/pool/main/e/edk2/ovmf_2022.11-6+deb12u2_all.deb
  tar -xf ovmf.deb && tar -xf data.tar.xz
  mkdir -p "$OVMF_DIR"
  cp usr/share/OVMF/OVMF_CODE_4M.secboot.fd "$OVMF_DIR/code.fd"
  cp usr/share/OVMF/OVMF_VARS_4M.fd "$OVMF_DIR/vars.fd"
  cd "$REPO_PWD"
fi

docker rm -f pxe-ext-ctl >/dev/null 2>&1 || true
docker run --rm -d --name pxe-ext-ctl --privileged \
  -v "$WORK:/work" -v "$ALPINE_ISO:/iso:ro" \
  alpine:3.22 sleep infinity >/dev/null
docker exec pxe-ext-ctl sh -c '
  apk add -q dnsmasq socat qemu-system-x86_64 iproute2 2>&1 | tail -1
  ip link add br-pxe type bridge; ip addr add '"$BRIDGE_IP"'/24 dev br-pxe
  ip link set br-pxe up
  ip tuntap add dev tap0 mode tap; ip link set tap0 master br-pxe; ip link set tap0 up
  cp /work/media/netboot/external-tftp/dnsmasq.conf.example /etc/dnsmasq-pxe.conf
  sed -i "s|tftp-root=.*|tftp-root=/work/media/netboot/external-tftp|; s|<tftp-server>|'"$BRIDGE_IP"'|g" /etc/dnsmasq-pxe.conf
  dnsmasq --conf-file=/etc/dnsmasq-pxe.conf --no-daemon --log-queries >/work/logs/dnsmasq.log 2>&1 &
  # guest HTTP reaches the host mammoth through this forward
  socat TCP-LISTEN:'"$HTTP_PORT"',bind='"$BRIDGE_IP"',fork,reuseaddr TCP:host.docker.internal:'"$HTTP_PORT"' &
  sleep 1
  qemu-img create -f raw /work/disk.raw 8G >/dev/null
  rm -f /work/logs/serial.log
  # q35: the pairing the 4M OVMF builds expect; TCG (no KVM on the mac)
  qemu-system-x86_64 -machine q35 -m 2048 -smp 2 -display none \
    -serial file:/work/logs/serial.log \
    -drive if=pflash,format=raw,readonly=on,file=/work/ovmf/code.fd \
    -drive if=pflash,format=raw,file=/work/ovmf/vars.fd \
    -drive file=/work/disk.raw,format=raw,if=none,id=disk0 \
    -device nvme,drive=disk0,serial=pxeextdisk \
    -netdev tap,id=n0,ifname=tap0,script=no,downscript=no \
    -device virtio-net-pci,netdev=n0,mac='"$GUEST_MAC"' \
    -no-reboot -daemonize -pidfile /work/qemu.pid
  echo "  ✓ guest booting (UEFI, TCG — several minutes)"
'

echo "· waiting for the install to complete (six stages)"
JOB_ID=$(curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs" | python3 -c "
import json,sys
for j in json.load(sys.stdin)['items']:
    if j['type']=='install': print(j['id']); break
")
FINAL=""
for i in $(seq 1 240); do
  STATE=$(curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs/$JOB_ID/tasks" | python3 -c "
import json,sys
items=json.load(sys.stdin)['items']
print(','.join(t['state'] for t in items) if items else '')" 2>/dev/null)
  case "$STATE" in
    succeeded|failed|canceled|*succeeded*|*failed*) FINAL="$STATE"; break;;
  esac
  sleep 5
done
echo "  task final: $FINAL"
curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs/$JOB_ID/tasks" | python3 -c "
import json,sys
for t in json.load(sys.stdin)['items']:
    print('  stages:', [(s['name'],s['state']) for s in t.get('stages',[])])
    e=t.get('error') or {}
    if e: print('  error:', e.get('code'), e.get('message','')[:200])
"
grep -a "mammoth-agent" "$WORK/logs/serial.log" 2>/dev/null | tail -6 | sed 's/^/  serial: /' || true
if echo "$FINAL" | grep -q succeeded; then echo "EXTERNAL PXE E2E ✓"; else echo "EXTERNAL PXE E2E ✗ (see $WORK/logs)"; exit 1; fi
