package builder

// miniproot — a minimal init environment appended to the netboot probe's
// initramfs as a second cpio segment. The kernel unpacks initramfs
// archives in order (a later file replaces an earlier one), so the
// appended /init REPLACES the alpine init entirely: the probe boot then
// runs a self-sufficient busybox flow — mount pseudo-fs, load the NIC
// driver, DHCP via busybox udhcpc, scan /sys, POST the report, power off —
// with no alpine init, no openrc and no apk involved at any stage.
//
// Why it exists: the disk-less root assembly runs apk in the initramfs and
// apk can fail invisibly there — --quiet (console flip) silences it and
// alpine init's `eend $?` takes a later umount's exit code — leaving
// /sysroot without busybox, openrc or /sbin/init; switch_root drops to its
// emergency shell and the probe never runs (office-PC 2026-11-13: ten
// boots, ten identical strandings). Replacing /init skips the failing
// stage instead of patching around it.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
)

// miniprootFile is the segment's name inside the boot tree (and the grant
// key for the machine-face file endpoint).
const miniprootFile = "miniproot.cpio.gz"

// miniprootSegment builds the appended initramfs segment. initrd is the
// carrier's main initramfs image (plain or gzip cpio) — busybox and the
// musl loader are extracted from it, so the segment always matches the
// carrier's architecture and libc; the NIC driver modules already live in
// the main initramfs and are insmod'd by the /init script.
func miniprootSegment(initrd []byte, reportURL, flavor string, debugShell bool) ([]byte, error) {
	src := initrd
	if zr, zerr := gzip.NewReader(bytes.NewReader(initrd)); zerr == nil {
		if plain, rerr := io.ReadAll(zr); rerr == nil {
			src = plain
		}
	}
	entries, err := cpioReadNewc(src)
	if err != nil {
		return nil, fmt.Errorf("builder: miniproot: carrier initramfs: %w", err)
	}
	var busybox, musl []byte
	for _, e := range entries {
		switch {
		case e.Name == "bin/busybox" || strings.HasSuffix(e.Name, "/busybox"):
			if len(busybox) == 0 {
				busybox = e.Body
			}
		case strings.Contains(e.Name, "ld-musl-"):
			if len(musl) == 0 {
				musl = e.Body
			}
		}
	}
	if len(busybox) == 0 {
		return nil, fmt.Errorf("builder: miniproot: no busybox in the carrier initramfs")
	}
	if len(musl) == 0 {
		return nil, fmt.Errorf("builder: miniproot: no musl loader in the carrier initramfs")
	}
	seg := cpioWriteNewc([]cpioEntry{
		{Name: "init", Mode: 0o100755, Body: []byte(miniprootInitScript(reportURL, flavor, debugShell))},
		{Name: "bin/", Mode: 0o040755},
		{Name: "bin/busybox", Mode: 0o100755, Body: busybox},
		{Name: "bin/sh", Mode: 0o120777, Link: "busybox"},
		{Name: "lib/", Mode: 0o040755},
		{Name: "lib/ld-musl-x86_64.so.1", Mode: 0o100755, Body: musl},
		{Name: "tmp/", Mode: 0o041777},
	})
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(seg); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// miniprootInitScript is the /init the appended segment installs: PID 1
// for the probe boot. Self-sufficient busybox flow — pseudo-fs mounts,
// NIC driver insmod, DHCP through an inline udhcpc script (a busybox-only
// root has no /usr/share/udhcpc helpers), /sys scan, report POST, power
// off; every failure lands in a shell on the console (tty0 under
// VGAConsole) instead of an invisible serial line.
func miniprootInitScript(reportURL, flavor string, debugShell bool) string {
	debug := ""
	if debugShell {
		debug = "/bin/sh < /dev/tty2 > /dev/tty2 2>&1 &\n"
	}
	return fmt.Sprintf(`#!/bin/sh
# mammoth miniproot /init (%s kernel): standalone probe boot — no alpine
# init, no openrc, no apk. Everything below is busybox.
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /tmp
echo "[miniproot] init: pseudo-fs mounted"

# NIC drivers ride the main initramfs (netboot initramfs carries the full
# ethernet set — the X722 machine-room finding); insmod by path, the
# modules.dep database is not guaranteed to be present.
for m in r8169 e1000e igb igc tg3 atlantic virtio_net vmxnet3; do
	modprobe "$m" 2>/dev/null
done
for ko in /lib/modules/*/kernel/drivers/net/ethernet/*/*.ko \
	/lib/modules/*/kernel/drivers/net/ethernet/*/*/*.ko; do
	insmod "$ko" 2>/dev/null
done
sleep 1
echo "[miniproot] drivers loaded; NICs: $(ls /sys/class/net 2>/dev/null | tr '\n' ' ')"

# DHCP through an inline script — a busybox-only root has no
# /usr/share/udhcpc/default.script to lean on. The initramfs's lease (if
# the ROM phase got one from this engine's pool) already configures eth0;
# udhcpc only adds what is missing.
cat > /tmp/udhcpc.sh <<'UDHCP'
#!/bin/sh
case "$1" in
bound|renew)
	[ -n "$ip" ] || exit 0
	PFX=$(ipcalc -p "$ip" "${subnet:-255.255.255.0}" 2>/dev/null | sed -n 's/PREFIX=//p')
	ip addr add "$ip/${PFX:-24}" dev "$iface" 2>/dev/null
	ip link set "$iface" up
	[ -n "$router" ] && ip route replace default via "$router" dev "$iface" 2>/dev/null
	;;
deconfig)
	ip addr flush dev "$iface" 2>/dev/null
	;;
esac
exit 0
UDHCP
chmod +x /tmp/udhcpc.sh
for n in /sys/class/net/*; do
	i="${n##*/}"
	[ "$i" = "lo" ] && continue
	ip link set "$i" up 2>/dev/null
	udhcpc -i "$i" -s /tmp/udhcpc.sh -n -q -t 4 -T 3 2>/dev/null
done
echo "[miniproot] network: $(ip -4 addr show 2>/dev/null | grep inet | tr '\n' ' ')"
%s
MAC="$(sed -n 's/.*enroll_mac=\([^ ]*\).*/\1/p' /proc/cmdline)"
URL="%s"
[ -n "$MAC" ] && URL="$URL?mac=$MAC"
OUT=/tmp/probe.json
{
	printf '{"captured_at":"%%s","source":"ramdisk","disks":[' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
	first=1
	for d in /sys/block/*; do
		n="${d##*/}"
		case "$n" in loop*|ram*|sr*|fd*|dm-*|md*) continue ;; esac
		[ -r "$d/size" ] || continue
		s=$(cat "$d/size") || continue
		model=$(sed 's/[\\"]//g; s/^ *//; s/ *$//' "$d/device/model" 2>/dev/null)
		serial=$(sed 's/[\\"]//g' "$d/device/serial" 2>/dev/null)
		[ $first -eq 1 ] && first=0 || printf ','
		printf '{"device":"%%s","size_bytes":%%d' "$n" $((s * 512))
		[ -n "$model" ] && printf ',"model":"%%s"' "$model"
		[ -n "$serial" ] && printf ',"serial":"%%s"' "$serial"
		printf '}'
	done
	printf '],"nics":['
	nf=1
	for x in /sys/class/net/*; do
		n="${x##*/}"
		[ "$n" = "lo" ] && continue
		m=$(cat "$x/address" 2>/dev/null)
		case "$m" in ""|"00:00:00:00:00:00") continue ;; esac
		c=$(cat "$x/carrier" 2>/dev/null)
		[ $nf -eq 1 ] && nf=0 || printf ','
		printf '{"name":"%%s","mac":"%%s","link_up":%%s}' "$n" "$m" "${c:-0}"
	done
	printf ']}\n'
} > $OUT
echo "[miniproot] scan done: $(wc -c < $OUT) bytes"
wget -q -T 15 -O /dev/null --post-file=$OUT "$URL" && echo "[miniproot] report delivered" && poweroff -f
echo "[miniproot] report failed — shell below"
exec /bin/sh
`, flavor, debug, reportURL)
}
