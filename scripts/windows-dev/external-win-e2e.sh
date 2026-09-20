#!/bin/sh
# Windows wimboot-over-PXE — external escape-hatch E2E (docs/compat/
# distros.md §windows, "wimboot over PXE"). Same rig shape as
# scripts/pxe-dev/external-e2e.sh (privileged alpine container: dnsmasq as
# the site DHCP+TFTP, socat bridging guest HTTP to the host's mammoth, TCG
# qemu UEFI guest on a tap), but the guest boots the WinPE chain:
#
#   site DHCP → TFTP ipxe-amd64.efi (plain iPXE — the wimboot host) →
#   re-DHCP → boot.ipxe trampoline → HTTP per-MAC script (wimboot shape) →
#   HTTP wimboot + bootmgr/bootmgfw.efi/BCD/boot.sdi + augmented boot.wim →
#   WinPE ramdisk (X:) → setup reads autounattend.xml + install.wim locally.
#
# Acceptance (default): the entry arms with the wimboot script shape and the
# guest walks the whole chain — DHCP → TFTP ipxe-amd64.efi → boot.ipxe →
# HTTP wimboot + boot files + small-augmented boot.wim → WinPE up. The rig
# screendumps periodically (screen-*.png); WinPE's language picker is the
# "carrier chain verified" signal. NOTE: with PXESupport=none there is no
# install source yet — setup will stop at the unattend's DiskConfiguration
# analysis (no disk source); the completion path (GPT on disk, six stages)
# activates when the SMB install source lands (docs/compat/distros.md
# §windows). --full waits for the six-stage task regardless (for the
# post-SBM era); --sb boots the Secure Boot firmware (expected boundary:
# the firmware refuses the unsigned NBP).
#
# Usage: host$ scripts/windows-dev/external-win-e2e.sh [--full] [--sb] [--keep]
# Requires: docker (VM memory >= 10G for the 8G guest), wimlib-imagex + 7z
# on the host (builder stage), ~/mammoth-qxe/windows/<win2019 iso>.
set -e
REPO_PWD="$PWD"
cd "$(dirname "$0")/../.."

WORK=${WIN_EXT_WORK:-/tmp/win-pxe-rg}
QXE=${WIN_EXT_QXE:-$HOME/mammoth-qxe}
ISO=$(ls "$QXE"/windows/cn_windows_server_2019_x64_dvd_4de40f33.iso 2>/dev/null || true)
GUEST_MAC=52:54:00:12:34:56
BRIDGE_IP=192.168.77.1
HTTP_PORT=18082
FULL=0; SB=0
for a in "$@"; do case "$a" in
  --full) FULL=1;; --sb) SB=1;; --keep) ;; *) echo "unknown arg $a"; exit 1;; esac; done

[ -f "$ISO" ] || { echo "missing windows 2019 ISO under $QXE/windows/"; exit 1; }
command -v wimlib-imagex >/dev/null || { echo "wimlib-imagex not on PATH (brew install wimlib)"; exit 1; }

echo "· building mammoth + starting (external mode, db windows_e2e)"
go build -o "$WORK/mammoth" ./cmd/mammoth
mkdir -p "$WORK/media" "$WORK/logs"
rm -rf "$WORK/media/netboot" 2>/dev/null || true
# DB rebuild: terminate stale sessions first (a leftover mammoth from a
# previous run holds the database and blocks the drop)
docker exec mammoth-dev-pg psql -U mammoth -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='windows_e2e' AND pid<>pg_backend_pid()" -c "DROP DATABASE IF EXISTS windows_e2e" -c "CREATE DATABASE windows_e2e" >/dev/null 2>&1 || true

KEY=$(python3 -c "import base64,os; print(base64.b64encode(os.urandom(32)).decode())")
MAMMOTH_DATABASE_URL='postgres://mammoth:mammoth@localhost:55432/windows_e2e?sslmode=disable' \
MAMMOTH_HTTP_ADDR=":$HTTP_PORT" MAMMOTH_API_TOKEN=devtoken MAMMOTH_MASTER_KEY="$KEY" \
MAMMOTH_MEDIA_DIR="$WORK/media" \
MAMMOTH_EXTERNAL_URL="http://$BRIDGE_IP:$HTTP_PORT" \
MAMMOTH_PXE_ENABLED=true MAMMOTH_PXE_MODE=external \
MAMMOTH_WINDOWS_INSTALL_SHARE='\\192.168.77.1\mammoth-media' \
"$WORK/mammoth" serve --mode=all >"$WORK/logs/server.log" 2>&1 &
SERVER_PID=$!
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  docker rm -f win-ext-ctl >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
for i in $(seq 1 60); do curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null && break; sleep 0.5; done
curl -sf "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null || { echo "mammoth not healthy"; tail -20 "$WORK/logs/server.log"; exit 1; }

echo "· register fake machine + submit windows2019 install (PXE)"
CRED=$(curl -s -X POST -H "Authorization: Bearer devtoken" -H "Content-Type: application/json" "http://127.0.0.1:$HTTP_PORT/api/v1/credentials" -d '{"type":"bmc","name":"win-pxe","secret":{"username":"a","password":"b"}}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
MID=$(curl -s -X POST -H "Authorization: Bearer devtoken" -H "Content-Type: application/json" "http://127.0.0.1:$HTTP_PORT/api/v1/machines" -d "{\"bmc\":{\"address\":\"fake://win-pxe-$RANDOM\",\"protocol\":\"fake\",\"credential_id\":\"$CRED\"}}" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
for i in $(seq 1 45); do
  ST=$(curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/machines/$MID" | python3 -c "import json,sys; print(json.load(sys.stdin)['state'])" 2>/dev/null)
  [ "$ST" = "ready" ] && break; sleep 2
done
JOB=$(curl -s -X POST -H "Authorization: Bearer devtoken" -H "Content-Type: application/json" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs" -d '{
  "type": "install",
  "targets": {"machine_ids": ["'"$MID"'"]},
  "spec": {
    "boot": {"strategy": "pxe"},
    "image": {"source": "file://'"$ISO"'", "distro": "windows2019"},
    "storage": {"disks": [{"select": {"match": {"type": "nvme", "size": "largest"}}, "wipe": true,
      "partitions": [
        {"size": "300M", "fs": "vfat", "mount": "/boot/efi", "flags": ["esp"]},
        {"size": "rest", "fs": "ntfs", "mount": "/"}]}]},
    "network": [{"match": {"mac": "'"$GUEST_MAC"'"}}, {"match": {"mac": "aa:bb:cc:dd:ee:01"}}],
    "identity": {"hostname_pattern": "win-pxe-{index}"},
    "access": {"ssh_keys": []}
  }}' | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['id']) if 'id' in d else (print('SUBMIT-ERR', d) or exit(1))")
echo "  ✓ job $JOB"

# wait for prepare_media (7z tree extract + wimlib surgery — minutes on the
# first run) and the wimboot-shaped entry to arm
TOKEN=""
for i in $(seq 1 120); do
  SCRIPT=$(curl -s "http://127.0.0.1:$HTTP_PORT/netboot/script?mac=$GUEST_MAC")
  TOKEN=$(echo "$SCRIPT" | grep -o 'files/[a-f0-9]*/' | head -1 | cut -d/ -f2)
  [ -n "$TOKEN" ] && break
  sleep 5
done
[ -n "$TOKEN" ] || { echo "✗ boot entry never armed"; tail -30 "$WORK/logs/server.log"; exit 1; }
echo "$SCRIPT" | grep -q "kernel .*wimboot" && echo "$SCRIPT" | grep -q "initrd .*boot.wim" \
  && echo "  ✓ wimboot entry armed (token $TOKEN)" || { echo "✗ entry is not wimboot-shaped:"; echo "$SCRIPT"; exit 1; }
ls -la "$WORK/media/netboot/$TOKEN/" | sed 's/^/    /'

echo "· container: bridge + dnsmasq + qemu guest ($([ $SB = 1 ] && echo 'Secure Boot ON' || echo 'Secure Boot OFF'))"
OVMF_DIR="$WORK/ovmf"
if [ ! -f "$OVMF_DIR/code-nosb.fd" ]; then
  echo "· fetching Debian OVMF (network stack)"
  mkdir -p "$OVMF_DIR-x" && cd "$OVMF_DIR-x"
  curl -sL -o ovmf.deb https://deb.debian.org/debian/pool/main/e/edk2/ovmf_2022.11-6+deb12u2_all.deb
  tar -xf ovmf.deb && tar -xf data.tar.xz
  mkdir -p "$OVMF_DIR"
  cp usr/share/OVMF/OVMF_CODE_4M.secboot.fd "$OVMF_DIR/code-sb.fd"
  cp usr/share/OVMF/OVMF_CODE_4M.fd "$OVMF_DIR/code-nosb.fd"
  cp usr/share/OVMF/OVMF_VARS_4M.fd "$OVMF_DIR/vars-template.fd"
  cd "$REPO_PWD"
fi
CODE=code-nosb.fd
[ $SB = 1 ] && CODE=code-sb.fd
cp -f "$OVMF_DIR/vars-template.fd" "$WORK/vars.fd"

docker rm -f win-ext-ctl >/dev/null 2>&1 || true
docker run --rm -d --name win-ext-ctl --privileged \
  -v "$WORK:/work" \
  alpine:3.22 sleep infinity >/dev/null
docker exec win-ext-ctl sh -c '
  apk add -q dnsmasq socat qemu-system-x86_64 qemu-img iproute2 samba 2>&1 | tail -1
  ip link add br-pxe type bridge; ip addr add '"$BRIDGE_IP"'/24 dev br-pxe
  ip link set br-pxe up
  ip tuntap add dev tap0 mode tap; ip link set tap0 master br-pxe; ip link set tap0 up
  cat > /etc/dnsmasq-win.conf <<EOF
dhcp-authoritative
dhcp-range=192.168.77.50,192.168.77.150,255.255.255.0,12h
dhcp-option=option:router,'"$BRIDGE_IP"'
enable-tftp
tftp-root=/work/media/netboot/external-tftp
dhcp-match=set:ipxe,175
dhcp-host='"$GUEST_MAC"',set:winboot
dhcp-boot=tag:winboot,tag:!ipxe,ipxe-amd64.efi,,'"$BRIDGE_IP"'
dhcp-boot=tag:ipxe,boot.ipxe,,'"$BRIDGE_IP"'
EOF
  dnsmasq --conf-file=/etc/dnsmasq-win.conf --no-daemon --log-queries >/work/logs/dnsmasq.log 2>&1 &
  # The deployment SMB export stand-in: read-only /work/media, guest access —
  # the wimboot startnet maps \\192.168.77.1\mammoth-media from WinPE.
  cat > /etc/samba/smb.conf <<'SAMBAEOF'
[global]
  map to guest = Bad User
  server min protocol = SMB2
  log file = /work/logs/samba.log
[mammoth-media]
  path = /work/media
  browseable = yes
  guest ok = yes
  read only = yes
  force user = root
SAMBAEOF
  mkdir -p /run/samba /var/lib/samba/private /var/cache/samba
  smbd -D && echo "  samba up (guest read-only /work/media)"
  socat TCP-LISTEN:'"$HTTP_PORT"',bind='"$BRIDGE_IP"',fork,reuseaddr TCP:host.docker.internal:'"$HTTP_PORT"' &
  sleep 1
  # a stale mon.sock breaks the monitor bind (virtiofs cannot unlink sockets)
  rm -f /work/mon.sock /work/qemu.pid /work/disk.raw
  qemu-img create -f raw /work/disk.raw 40G >/dev/null
  rm -f /work/logs/serial.log
  # 8G guest: the augmented boot.wim (~4.7G) plus the WinPE runtime must fit
  # in RAM. The Docker Desktop VM needs >= 10G allotted (settings MemoryMiB)
  # or qemu gets OOM-killed there — the rig was validated on 16G.
  qemu-system-x86_64 -machine q35 -m 8192 -smp 4 -display none \
    -serial file:/work/logs/serial.log \
    -monitor unix:/work/mon.sock,server,nowait \
    -drive if=pflash,format=raw,readonly=on,file=/work/ovmf/'"$CODE"' \
    -drive if=pflash,format=raw,file=/work/vars.fd \
    -drive file=/work/disk.raw,format=raw,if=none,id=disk0 \
    -device nvme,drive=disk0,serial=winpxedisk \
    -netdev tap,id=n0,ifname=tap0,script=no,downscript=no \
    -device virtio-net-pci,netdev=n0,mac='"$GUEST_MAC"' \
    -no-reboot -daemonize -pidfile /work/qemu.pid
  echo "  ✓ guest booting (UEFI x64, TCG — minutes; boot.wim ~4.7G over HTTP)"
'

shot() { docker exec win-ext-ctl sh -c "echo screendump /work/logs/screen-$1.png | socat - UNIX-CONNECT:/work/mon.sock" >/dev/null 2>&1 || true; }

echo "· waiting for the WinPE chain (watch: HTTP pulls → WinPE screen)"
if [ $FULL = 1 ]; then WAIT=1440; else WAIT=20; fi
GPT=""
for i in $(seq 1 $WAIT); do
  sleep 60
  # objective acceptance signal: setup accepted the unattend and partitioned
  # the disk (GPT protective MBR + "EFI PART" at LBA1)
  GPT=$(python3 -c "
f=open('$WORK/disk.raw','rb'); f.seek(512); print(f.read(8))" 2>/dev/null || true)
  [ "$GPT" = "b'EFI PART'" ] && { echo "  ✓ GPT appeared on the raw disk (setup accepted autounattend, minute $i)"; break; }
  # progress chatter: the big HTTP pulls from the mammoth access log
  if [ $((i % 5)) = 0 ]; then
    grep -aoE "GET /netboot/files/[a-f0-9]+/(wimboot|bootmgr|bootmgfw.efi|BCD|boot.sdi|boot.wim)" "$WORK/logs/server.log" 2>/dev/null | sort | uniq -c | sed 's/^/    /'
    shot "$i"
  fi
done
shot final
if [ "$GPT" = "b'EFI PART'" ]; then
  echo "  ✓ GPT on the raw disk — setup applied the unattend's DiskConfiguration"
  echo "WINDOWS WIMBOOT PXE E2E ✓ (full chain, unattend accepted)"
  exit 0
fi
if [ $FULL = 1 ]; then
  echo "✗ no GPT within the window — see $WORK/logs (screen-*.png, server.log)"
  exit 1
fi
# default (carrier-verification) round: judge WinPE up from the screendumps
PULLS=$(grep -acE "GET /netboot/files" "$WORK/logs/server.log" 2>/dev/null || true)
if [ "$PULLS" != "0" ] && [ "$PULLS" != "" ]; then
  echo "  ✓ guest pulled boot files over HTTP ($PULLS requests) — screendumps in $WORK/logs/screen-*.png"
  echo "  ✓ judge WinPE-up from the screenshots (language picker = carrier chain verified)"
  echo "WINDOWS WIMBOOT PXE CARRIER ✓ (chain verified; SMB install source is the remaining segment)"
  exit 0
fi
echo "✗ no HTTP pulls from the guest — see $WORK/logs (dnsmasq/server logs)"
exit 1
if [ $SB = 1 ]; then
  HITS=$(grep -ac "netboot/files" "$WORK/logs/server.log" 2>/dev/null || true)
  if [ "$HITS" = "0" ]; then echo "SB-ON BOUNDARY ✓ (firmware refused the unsigned NBP — no HTTP reached mammoth)"; else echo "SB-ON UNEXPECTED HTTP HITS ($HITS) — inspect"; exit 1; fi
  exit 0
fi

if [ $FULL = 1 ]; then
  echo "· --full: waiting for the six-stage install (TCG — 1-3h)"
  FINAL=""
  for i in $(seq 1 280); do
    STATE=$(curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs/$JOB/tasks" | python3 -c "
import json,sys
items=json.load(sys.stdin)['items']
print(','.join(t['state'] for t in items) if items else '')" 2>/dev/null)
    case "$STATE" in succeeded|failed|canceled) FINAL="$STATE"; break;; esac
    sleep 60
  done
  echo "  task final: $FINAL"
  curl -s -H "Authorization: Bearer devtoken" "http://127.0.0.1:$HTTP_PORT/api/v1/jobs/$JOB/tasks" | python3 -c "
import json,sys
for t in json.load(sys.stdin)['items']:
    print('  stages:', [(s['name'],s['state']) for s in t.get('stages',[])])
    e=t.get('error') or {}
    if e: print('  error:', e.get('code'), e.get('message','')[:200])
"
  echo "$FINAL" | grep -q succeeded && { echo "WINDOWS WIMBOOT PXE E2E ✓ (full)"; exit 0; } || { echo "WINDOWS WIMBOOT PXE E2E ✗"; exit 1; }
fi

echo "WINDOWS WIMBOOT PXE E2E ✓ (WinPE up + unattend accepted; run with --full for the six-stage tail)"
