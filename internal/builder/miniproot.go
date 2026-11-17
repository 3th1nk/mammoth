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
//
// Diagnosability: every milestone goes to the console AND to the engine's
// syslog sink via busybox logger (the engine logs remote datagrams with
// the source IP) — a machine with no serial and no operator can still be
// diagnosed engine-side. The report POST retries every 30s: an engine-side
// fix landing mid-retry is picked up without a reboot.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
func miniprootSegment(initrd []byte, storage []cpioEntry, reportURL, flavor string, debugShell bool, syslogPort int) ([]byte, error) {
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
	seg := []cpioEntry{
		{Name: "init", Mode: 0o100755, Body: []byte(miniprootInitScript(reportURL, flavor, debugShell, syslogPort))},
		{Name: "bin/", Mode: 0o040755},
		{Name: "bin/busybox", Mode: 0o100755, Body: busybox},
		{Name: "bin/sh", Mode: 0o120777, Link: "busybox"},
		{Name: "lib/", Mode: 0o040755},
		{Name: "lib/ld-musl-x86_64.so.1", Mode: 0o100755, Body: musl},
		{Name: "tmp/", Mode: 0o041777},
	}
	// Storage drivers: the netboot initramfs carries network drivers only —
	// ata/ahci/sd_mod/nvme live in the modloop (SquashFS), which the
	// miniproot flow never mounts (the alpine openrc path that fetches it
	// was replaced). The caller extracts them from the boot tree's modloop;
	// the /init script insmods the chain in dependency order. Best-effort:
	// failure downgrades the probe to NIC-only reporting.
	seg = append(seg, storage...)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	packed := cpioWriteNewc(seg)
	if _, err := gw.Write(packed); err != nil {
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
// root has no /usr/share/udhcpc helpers), /sys scan, report POST with
// 30s retry (engine fixes land mid-retry), power off; every failure lands
// in a shell on the console (tty0 under VGAConsole) instead of an
// invisible serial line. Milestones go to the engine's syslog sink too —
// a machine with no serial and no operator is still diagnosed engine-side.
// Sprintf verbs, in order: flavor, debug-line, logd-line, report URL.
func miniprootInitScript(reportURL, flavor string, debugShell bool, syslogPort int) string {
	debugLine := ""
	if debugShell {
		debugLine = "/bin/sh < /dev/tty2 > /dev/tty2 2>&1 &\n"
	}
	// The sink host is the engine the reportURL points at — the same machine
	// the report POST reaches. An earlier hardcoded address shipped probe
	// boots that announced their milestones to a foreign engine's sink (the
	// .250 machine-room host) from every other test segment. Hostless
	// report URL → milestones stay console-only.
	logd := `LOGD() { echo "[miniproot] $*"; }`
	if u, err := url.Parse(reportURL); err == nil && u.Hostname() != "" {
		if syslogPort == 0 {
			syslogPort = 514
		}
		logd = fmt.Sprintf(`LOGD() { logger -h %s -P %d -t miniproot "$*" 2>/dev/null; echo "[miniproot] $*"; }`,
			u.Hostname(), syslogPort)
	}
	return fmt.Sprintf(`#!/bin/sh
# mammoth miniproot /init (%s kernel): standalone probe boot — no alpine
# init, no openrc, no apk. Everything below is busybox. The first steps use
# absolute /bin/busybox calls: PATH and applet links only exist after
# busybox --install, which itself needs /proc mounted to resolve itself.
export PATH=/bin:/sbin
/bin/busybox mkdir -p /proc /sys /dev /tmp
/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
/bin/busybox mount -t devtmpfs devtmpfs /dev 2>/dev/null
/bin/busybox --install -s /bin
%s
LOGD "init: pseudo-fs mounted, applets linked"
%s

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

# Storage drivers ride the appended segment (extracted from the modloop —
# the netboot initramfs carries network drivers only). Dependency order:
# scsi_common -> scsi_mod -> libata -> libahci -> ahci -> sd_mod, nvme-core
# -> nvme; the sweep catches the remaining SATA HBA modules.
KDIR=$(ls -d /lib/modules/*/mammoth-storage 2>/dev/null | head -1)
LOAD=/tmp/storage.loaded
: > $LOAD
if [ -n "$KDIR" ]; then
	for m in scsi_common scsi_mod libata libahci ahci sd_mod nvme-core nvme; do
		insmod "$KDIR/$m.ko" 2>/dev/null && echo "$m.ko" >> $LOAD
	done
	for pass in 1 2 3 4 5; do
		progress=0
		for ko in "$KDIR"/*.ko; do
			b=$(basename "$ko")
			grep -qx "$b" $LOAD 2>/dev/null && continue
			insmod "$ko" 2>/dev/null && { echo "$b" >> $LOAD; progress=1; }
		done
		[ "$progress" = 1 ] || break
	done
fi
LOGD "drivers loaded; NICs: $(ls /sys/class/net 2>/dev/null | tr '\n' ' '); disks: $(ls /sys/block 2>/dev/null | tr '\n' ' '); storage: $(wc -l < $LOAD) modules"

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
	ip addr add "$ip/${PFX:-24}" dev "$interface" 2>/dev/null
	ip link set "$interface" up
	[ -n "$router" ] && ip route replace default via "$router" dev "$interface" 2>/dev/null
	;;
deconfig)
	ip addr flush dev "$interface" 2>/dev/null
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
LOGD "network: $(ip -4 addr show 2>/dev/null | grep inet | tr '\n' ' ')"
MAC="$(sed -n 's/.*enroll_mac=\([^ ]*\).*/\1/p' /proc/cmdline)"
URL="%s"
[ -n "$MAC" ] && URL="$URL?mac=$MAC"
LOGD "enroll_mac: [$MAC]; report URL: $URL"
OUT=/tmp/probe.json
{
	printf '{"captured_at":"%%s","source":"ramdisk","disks":[' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)"
	first=1
	for d in /sys/block/*; do
		n="${d##*/}"
		case "$n" in loop*|ram*|sr*|fd*|dm-*|md*) continue ;; esac
		[ -r "$d/size" ] || continue
		s=$(cat "$d/size") || continue
		# Serial sourcing, per bus (no smartmontools — the kernel sysfs
		# already exports every identifier the disk inventory needs):
		#   ATA/SATA (libata): device/serial + device/model
		#   NVMe:              device/serial (kernel nvme core exports it
		#                      on the block device's device dir too)
		#   everything else:   device/wwid (SCSI layer unique id — USB
		#                      bridges, SAS expanders; not a vendor serial
		#                      but still a stable unique discriminator)
		model=$(sed 's/[\\"]//g; s/^ *//; s/ *$//' "$d/device/model" 2>/dev/null)
		serial=$(sed 's/[\\"]//g; s/^ *//; s/ *$//' "$d/device/serial" 2>/dev/null)
		wwid=$(sed 's/[\\"]//g' "$d/device/wwid" 2>/dev/null)
		[ $first -eq 1 ] && first=0 || printf ','
		printf '{"device":"%%s","size_bytes":%%d' "$n" $((s * 512))
		[ -n "$model" ] && printf ',"model":"%%s"' "$model"
		[ -n "$serial" ] && printf ',"serial":"%%s"' "$serial"
		[ -n "$wwid" ] && printf ',"wwid":"%%s"' "$wwid"
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
		lu=true; [ "$c" = "0" ] && lu=false
		[ $nf -eq 1 ] && nf=0 || printf ','
		printf '{"name":"%%s","mac":"%%s","link_up":%%s}' "$n" "$m" "$lu"
	done
	printf ']}\n'
} > $OUT
LOGD "scan done: $(wc -c < $OUT) bytes; content: $(cat $OUT)"

# Report POST with retry: an engine-side fix landing mid-retry is picked up
# without a reboot (30s cadence, ~10 min window).
try=0
while [ $try -lt 20 ]; do
	try=$((try+1))
	if wget -q -T 15 -O /dev/null --post-file=$OUT "$URL"; then
		LOGD "report delivered (attempt $try)"
		sleep 8
		poweroff -f
	fi
	LOGD "report attempt $try failed"
	sleep 30
done
echo "[miniproot] report failed — shell below"
exec /bin/sh
`, flavor, debugLine, logd, reportURL)
}

// storageDriverEntries extracts the storage module set from the boot tree's
// modloop (SquashFS — the netboot initramfs carries network drivers only,
// ata/ahci/sd_mod/nvme live here) into flat entries under
// lib/modules/<kver>/mammoth-storage/. Uses the engine image's 7z (p7zip
// reads SquashFS natively). The /init script insmods the known storage
// chain in dependency order (scsi_common → scsi_mod → libata → libahci →
// ahci → sd_mod → nvme-core → nvme), then sweeps the directory for the
// remaining HBAs.
func storageDriverEntries(modloopPath string) ([]cpioEntry, error) {
	if _, err := os.Stat(modloopPath); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "mammoth-modloop-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	// modloop paths carry NO lib/ prefix (modules/<kver>/kernel/... — the
	// alpine netboot modloop layout).
	x := exec.Command("7z", "x", "-y", "-o"+tmp, modloopPath,
		"modules/*/kernel/drivers/ata/*",
		"modules/*/kernel/drivers/nvme/host/*",
		"modules/*/kernel/drivers/nvme/common/*",
		"modules/*/kernel/drivers/scsi/common/*",
		"modules/*/kernel/drivers/scsi/scsi_common.ko",
		"modules/*/kernel/drivers/scsi/scsi_mod.ko",
		"modules/*/kernel/drivers/scsi/sd_mod.ko")
	out, err := x.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("7z modloop extract: %w: %s", err, tail(out, 200))
	}
	kvers, err := os.ReadDir(filepath.Join(tmp, "modules"))
	if err != nil || len(kvers) == 0 {
		return nil, fmt.Errorf("7z modloop extract: no kernel module tree found")
	}
	kver := kvers[0].Name()
	src := filepath.Join(tmp, "modules", kver, "mammoth-storage")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return nil, err
	}
	for _, dir := range []string{"ata", "nvme/host", "nvme/common", "scsi", "scsi/common"} {
		matches, _ := filepath.Glob(filepath.Join(tmp, "modules", kver, "kernel", "drivers", dir, "*.ko"))
		for _, m := range matches {
			base := filepath.Base(m)
			if err := os.Rename(m, filepath.Join(src, base)); err != nil && !os.IsExist(err) {
				return nil, err
			}
		}
	}
	files, err := os.ReadDir(src)
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("7z modloop extract: no storage modules matched")
	}
	var entries []cpioEntry
	entries = append(entries, cpioEntry{Name: "lib/modules/" + kver + "/mammoth-storage/", Mode: 0o040755})
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(src, f.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, cpioEntry{
			Name: "lib/modules/" + kver + "/mammoth-storage/" + f.Name(),
			Mode: 0o100755, Body: body,
		})
	}
	return entries, nil
}
