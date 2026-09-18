#!/bin/bash
# UOS (customized anaconda) Finish-crash reproduction rig — scripts/uos-dev/.
#
# Background: uniontechos is blocked on a crash inside UOS's customized
# anaconda Finish task group (dasbus DBusError "max() arg is an empty
# sequence", task_proxy.Finish — docs/compat/distros.md §uniontechos).
# The crash is installer-internal, so reproducing it in qemu (no real
# hardware needed) unlocks two things: anchoring the traceback for a UOS
# report, and screening newer UOS ISOs (1050u1a/u2a) without spending a
# real-machine window.
#
# install phase: qemu boots the ISO's own kernel/initrd directly (-kernel,
# bypassing the boot menu so anaconda parameters flow via -append), the
# graphical installer runs on VGA with -display none, the kickstart comes
# from the host over slirp http (10.0.2.2), logs come over serial console
# and inst.syslog, and inst.sshd + hostfwd leave a way into the installer
# environment for /tmp/anaconda-tb-*. -no-reboot: anaconda's completion
# reboot kills qemu (rc=0) — that IS the pass signal; a hang is the crash
# (confirm with uos-shot.sh).
#
# boot phase: boot the installed disk only (meaningful when install
# completed) — the real-machine pass bar "reboot unattended" in miniature.
#
# Usage:
#   UOS_ISO=/path/to/uniontechos-server-20-1050a-amd64.iso scripts/uos-dev/uos-qemu.sh install
#   scripts/uos-dev/uos-qemu.sh shot     # screendump → PNG (crash dialog check)
#   scripts/uos-dev/uos-qemu.sh boot     # boot installed disk
#   scripts/uos-dev/uos-qemu.sh stop     # kill qemu/http/syslog helpers
set -euo pipefail

MODE=${1:-install}
ISO=${UOS_ISO:-$HOME/mammoth-qxe/uos/uniontechos-server-20-1050a-amd64.iso}
WORK=${UOS_WORK:-$HOME/mammoth-qxe/uos-run}
KS=${UOS_KS:-$(dirname "$0")/uos.ks}
HTTP_PORT=8086
SYSLOG_PORT=5141
QEMU=${QEMU:-qemu-system-x86_64}

case "$MODE" in
  install)
    [ -f "$ISO" ] || { echo "uos: ISO not found: $ISO" >&2; exit 2; }
    [ -f "$KS" ] || { echo "uos: kickstart not found: $KS" >&2; exit 2; }
    # A leftover qemu (e.g. hung on a crash dialog from an earlier round)
    # holds the hostfwd port and silently kills this run at startup.
    if lsof -nP -iTCP:$HTTP_PORT -sTCP:LISTEN >/dev/null 2>&1 || \
       lsof -nP -iTCP:2222 -sTCP:LISTEN >/dev/null 2>&1; then
      echo "uos: port 2222/$HTTP_PORT busy — a leftover qemu or http helper is" \
           "still up (kill it or run '$0 stop' in its WORK)" >&2
      exit 4
    fi
    # Fresh run, but keep any pre-extracted boot files (macOS usually cannot
    # mount these ISOs — see the extraction note below).
    KEEP=$(mktemp -d)
    for f in vmlinuz initrd.img; do
      [ -f "$WORK/$f" ] && cp "$WORK/$f" "$KEEP/"
    done
    rm -rf "$WORK"
    mkdir -p "$WORK/logs"
    for f in "$KEEP"/*; do [ -f "$f" ] && cp "$f" "$WORK/"; done
    rm -rf "$KEEP"
    cd "$WORK"
    cp "$KS" uos.ks

    # Volume label from the ISO 9660 PVD (offset 32808, 32 bytes, space pad)
    # — anaconda resolves inst.stage2=hd:LABEL=<label> against it.
    LABEL=$(dd if="$ISO" bs=1 skip=32808 count=32 2>/dev/null | tr -d ' ')
    echo "[uos] volume label: $LABEL"

    # Installer kernel+initrd straight off the ISO — same tree as stage2.
    # macOS hdiutil often cannot mount these hybrid/UDF-shaped UOS ISOs
    # ("无可装载的文件系统"); when that happens, extract the two files on
    # any Linux host (mount -o loop,ro → images/pxeboot/) and drop them
    # here — the boot files are all this phase needs from the ISO.
    if [ -f vmlinuz ] && [ -f initrd.img ]; then
      echo "[uos] reusing pre-extracted vmlinuz/initrd.img"
    else
      MNT=$(mktemp -d)
      if ! hdiutil attach -readonly -nobrowse -mountpoint "$MNT" "$ISO" >/dev/null 2>&1; then
        echo "uos: macOS cannot mount this ISO and no pre-extracted boot files in $WORK" \
             "(need vmlinuz + initrd.img; see scripts/uos-dev/README.md)" >&2
        exit 3
      fi
      cp "$MNT/images/pxeboot/vmlinuz" "$MNT/images/pxeboot/initrd.img" .
      hdiutil detach "$MNT" >/dev/null
    fi
    echo "[uos] kernel+initrd ready"

    qemu-img create -f qcow2 disk.qcow2 ${UOS_DISK_GB:-100}G >/dev/null

    # Media shape: iBMC virtual media enumerates as a USB CD-ROM, plain qemu
    # -cdrom as SATA — the single variable that differs between a real 2288H
    # run and the first qemu anchor run (which did NOT reproduce the crash).
    # UOS_MEDIA=usb|sata (default usb — the shape the real machine saw).
    MEDIA_ARGS="-cdrom $ISO"
    [ "${UOS_MEDIA:-usb}" = usb ] && \
      MEDIA_ARGS="-device qemu-xhci,id=xhci -drive if=none,id=cd0,format=raw,media=cdrom,readonly=on,file=$ISO -device usb-storage,drive=cd0,bus=xhci.0"

    # Host-side helpers: kickstart http + installer syslog sink.
    (cd "$WORK" && python3 -m http.server $HTTP_PORT --bind 0.0.0.0 >/dev/null 2>&1 & echo $! > http.pid)
    (nc -u -l $SYSLOG_PORT > logs/installer-syslog.log & echo $! > syslog.pid)

    # -no-reboot: anaconda's completion reboot terminates qemu — that is the
    # pass signal. A crash leaves the installer on its crash dialog: qemu
    # stays up, confirm via `uos-qemu.sh shot`.
    "$QEMU" -machine q35 -cpu max -smp 4 -m 4096 \
      -drive file=disk.qcow2,if=virtio,format=qcow2 \
      $MEDIA_ARGS \
      -netdev user,id=n0,hostfwd=tcp::2222-:22 -device virtio-net-pci,netdev=n0 \
      -kernel vmlinuz -initrd initrd.img \
      -append "inst.stage2=hd:LABEL=$LABEL inst.ks=http://10.0.2.2:$HTTP_PORT/uos.ks inst.syslog=10.0.2.2:$SYSLOG_PORT inst.sshd console=ttyS0,115200n8 ${UOS_EXTRA_ARGS:-}" \
      -serial file:logs/serial.log \
      -display none -vga std \
      -monitor unix:qmon.sock,server,nowait \
      -no-reboot &
    echo $! > qemu.pid
    echo "[uos] install phase running (pid $(cat qemu.pid))"
    echo "  tail -f $WORK/logs/serial.log"
    echo "  $0 shot     # screendump → PNG"
    echo "  ssh -p 2222 root@localhost   # installer env (inst.sshd), grab /tmp/anaconda-tb-*"
    ;;

  boot)
    cd "$WORK"
    "$QEMU" -machine q35 -cpu max -smp 4 -m 4096 \
      -drive file=disk.qcow2,if=virtio,format=qcow2 \
      -netdev user,id=n0,hostfwd=tcp::2222-:22 -device virtio-net-pci,netdev=n0 \
      -serial file:logs/boot-serial.log \
      -display none -vga std \
      -no-reboot &
    echo $! > qemu-boot.pid
    echo "[uos] booting installed disk (pid $(cat qemu-boot.pid))"
    echo "  ssh -p 2222 root@localhost  # password from the kickstart"
    ;;

  shot)
    cd "$WORK"
    "${QEMU%qemu-system-x86_64}"true >/dev/null 2>&1 || true
    echo "screendump screen.ppm" | nc -U qmon.sock >/dev/null
    sleep 1
    sips -s format png screen.ppm --out screen.png >/dev/null 2>&1
    echo "[uos] screendump: $WORK/screen.png"
    ;;

  stop)
    cd "$WORK"
    for f in qemu.pid qemu-boot.pid http.pid syslog.pid; do
      [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null && rm -f "$f"
    done
    pkill -f "http.server $HTTP_PORT" 2>/dev/null || true
    echo "[uos] stopped"
    ;;

  *)
    echo "usage: $0 install|boot|shot|stop" >&2
    exit 2
    ;;
esac
