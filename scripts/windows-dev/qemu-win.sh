#!/usr/bin/env bash
# windows-dev qemu rig — the BOOT-side verification harness, companion to the
# build-chain Go harness (internal/builder/windows_dev_test.go). Covers:
# OVMF -> bootmgfw -> WinPE -> setup with monitor control, screendump
# watchdog, and a completion-callback sink for the task.json -> SetupComplete
# chain. Runs TCG when /dev/kvm is absent (CentOS 8 qemu-kvm layout handled).
# Env: WIN_ISO (default $WIN_DIR/boot-dev.iso), WIN_DIR (default
# /data/mammoth/win-dev — the deployment host layout).
set -euo pipefail
DIR=${WIN_DIR:-/data/mammoth/win-dev}
ISO=${WIN_ISO:-$DIR/boot-dev.iso}
QEMU=$(command -v qemu-system-x86_64 || echo /usr/libexec/qemu-kvm)
MON=$DIR/qemu/mon

mon() { echo "$2" | python3 -c "import socket,sys,time; s=socket.socket(socket.AF_UNIX); s.connect('$MON'); s.sendall(sys.stdin.buffer.read()); time.sleep(2); s.close"; }

boot() {
  mkdir -p $DIR/qemu
  [ -f $DIR/qemu/disk.raw ] || qemu-img create -f raw $DIR/qemu/disk.raw 64G >/dev/null
  cp -f /usr/share/edk2/ovmf/OVMF_VARS.fd $DIR/qemu/VARS.fd
  nohup $QEMU -m 4096 -smp 4 \
    -drive file=$DIR/qemu/disk.raw,format=raw,if=ide \
    -cdrom $ISO -boot d \
    -drive if=pflash,format=raw,readonly=on,file=/usr/share/edk2/ovmf/OVMF_CODE.cc.fd \
    -drive if=pflash,format=raw,file=$DIR/qemu/VARS.fd \
    -nic user,model=e1000 \
    -vga std -display none \
    -monitor unix:$MON,server,nowait \
    >$DIR/qemu/qemu.log 2>&1 &
  sleep 2; pgrep -fc qemu-kvm && echo "BOOTED ($ISO)"
}

keys() {  # launch bootx64.efi from the UEFI shell + spam space across the
  # media's standard "press any key to boot" window
  MON=$MON python3 - <<'PYIN'
import socket, time, os
s = socket.socket(socket.AF_UNIX); s.connect(os.environ["MON"])
bs = chr(92); keys = "fs0:" + bs + "efi" + bs + "boot" + bs + "bootx64.efi"
m = {bs: "backslash", ":": "shift-semicolon", ".": "dot"}
for ch in keys:
    s.sendall(("sendkey %s\n" % m.get(ch, ch)).encode()); time.sleep(0.05)
s.sendall(b"sendkey ret\n"); time.sleep(2)
for i in range(60):
    s.sendall(b"sendkey spc\n"); time.sleep(0.5)
print("KEYS_SENT")
PYIN
}

dump() { mon "$1" "screendump $DIR/qemu/d$1.png"; echo "$DIR/qemu/d$1.png"; }

watch() {  # $2 rounds x 3 min screendump watchdog
  local n=${2:-60}
  nohup bash -c "for i in \$(seq 1 $n); do sleep 180; echo screendump $DIR/qemu/w\$i.png | python3 -c \"import socket,sys,time; s=socket.socket(socket.AF_UNIX); s.connect('$MON'); s.sendall(sys.stdin.buffer.read()); time.sleep(2); s.close\"; done" >/dev/null 2>&1 &
  echo "WATCHDOG $n rounds"
}

sink() {  # completion-callback sink: log every POST (task.json -> SetupComplete)
  python3 - > $DIR/qemu/sink.log 2>&1 <<'PYIN' &
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0)); body = self.rfile.read(n)
        print("CALLBACK", self.path, body.decode(errors="replace"), flush=True)
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass
HTTPServer(("0.0.0.0", 8080), H).serve_forever()
PYIN
  echo "SINK :8080 -> $DIR/qemu/sink.log"
}

killvm() { pkill -f "qemu-kvm|qemu-system" || true; sleep 2; echo KILLED; }
logf() { tail -3 $DIR/qemu/qemu.log; tail -3 $DIR/qemu/sink.log 2>/dev/null; }

case "${1:-}" in
  boot) boot;; keys) keys;; dump) dump "${2:?n}";; watch) watch "$@";;
  sink) sink;; kill) killvm;; log) logf;;
  cycle) killvm; sink; boot; sleep 8; keys; watch "${2:-60}";;
  *) echo "usage: $0 {boot|keys|dump <n>|watch <rounds>|sink|kill|log|cycle [rounds]}"; exit 1;;
esac
