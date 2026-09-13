// Ramdisk probe media (docs/05-inventory.md §4): a task-specific ISO that
// boots the target machine into a minimal live environment, scans /sys and
// POSTs the layout to the mammoth machine face, then powers off. Alpine
// standard is the carrier: the lts kernel + modloop carry the full
// real-server storage/network drivers (the probe's entire purpose), busybox
// covers networking and reporting with zero tooling dependencies — the same
// shape as Tinkerbell's HookOS, which is likewise an alpine netboot payload.
//
// Build strategy is the FULL REPACK the casper/debian-di install media use:
// the boot media must carry the distribution's /apks boot repository — the
// initramfs installs alpine-base from it into the memory root (a selective
// boot-chain assembly leaves /sbin/init missing). The probe logic rides in
// an apkovl overlay baked at the ISO root (apkovl= pins the name); openrc's
// local service runs it.

package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ProbeOptions configure one probe ISO build.
type ProbeOptions struct {
	// ISOPath is the local alpine standard ISO (the source tree is repacked).
	ISOPath string
	// OutputPath is where the assembled probe ISO is written (probe-<token>.iso).
	OutputPath string
	// WorkDir holds intermediate files; empty = OutputPath dir + ".build".
	WorkDir string
	// XorrisoPath overrides the xorriso binary (default: PATH lookup).
	XorrisoPath string
	// ReportURL is the machine-face endpoint the probe POSTs its findings to
	// (…/render/<token>/probe-report); the unguessable token in it is the
	// probe's credential.
	ReportURL string
	// StaticCIDR, when set (e.g. "198.51.100.75/24"), is applied to a NIC as
	// the DHCP fallback — machine rooms without a DHCP service are the norm
	// (install specs declare static networks too). Callers should prefer the
	// machine's own ssh.address as the source; the report URL may be in a
	// different subnet, in which case StaticGateway carries the return path.
	StaticCIDR string
	// StaticGateway, when set, becomes the default route of the static
	// fallback (cross-subnet report targets need it; same-subnet does not).
	StaticGateway string
	// Timeout bounds the whole build (default 10m).
	Timeout time.Duration
}

// BuildProbeISO assembles the ramdisk probe medium: the alpine ISO repacked
// with the probe's apkovl overlay and boot entries pinning it.
func BuildProbeISO(ctx context.Context, opt ProbeOptions) (string, error) {
	if opt.ISOPath == "" {
		return "", fmt.Errorf("builder: alpine ISO path is required")
	}
	if opt.OutputPath == "" {
		return "", fmt.Errorf("builder: output path is required")
	}
	if opt.ReportURL == "" {
		return "", fmt.Errorf("builder: report URL is required")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	xorriso := opt.XorrisoPath
	if xorriso == "" {
		xorriso = "xorriso"
	}

	// Kernel flavor resolved from the image (lts on standard, virt on virt) —
	// never hardcoded, the boot entries reference it.
	kernel, err := alpineKernel(ctx, xorriso, opt.ISOPath)
	if err != nil {
		return "", err
	}

	// apkovl overlay: the probe script + its openrc hookup, baked at the ISO
	// root where the initramfs applies it (apkovl= pins the exact name).
	overlay, err := probeOverlay(opt.ReportURL, opt.StaticCIDR, opt.StaticGateway, kernel)
	if err != nil {
		return "", err
	}

	// Serial console (BMC SOL reads ttyS0; listed last so init's output
	// follows it under qemu -nographic too) + the distribution's module
	// arguments. The overlay is NOT pinned via apkovl=: a bare filename
	// lands in prepare_apkovl's device:path parser and resolves to a
	// nonexistent initramfs-relative path (silently skipped). Omitted, the
	// initramfs auto-detects *.apkovl.tar.gz on the boot media root.
	kernelArgs := "modules=loop,squashfs,sd-mod,usb-storage console=tty0 console=ttyS0,115200"
	return rebuildPatchedISO(ctx, BootMediaOptions{
		ISOPath:     opt.ISOPath,
		OutputPath:  opt.OutputPath,
		WorkDir:     opt.WorkDir,
		XorrisoPath: opt.XorrisoPath,
		SeedFiles:   map[string]string{"mammoth.apkovl.tar.gz": string(overlay)},
		Timeout:     opt.Timeout,
	}, kernelArgs, layoutAlpine, kernel)
}

// alpineKernel resolves the kernel flavor name ("lts", "virt") from the
// image's boot tree.
func alpineKernel(ctx context.Context, xorriso, iso string) (string, error) {
	out, err := exec.CommandContext(ctx, xorriso, "-indev", iso, "-find", "/boot", "-type", "f").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("probe alpine boot tree: %w: %s", err, tail(out, 300))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.Trim(strings.TrimSpace(line), "'\"")
		if k, ok := strings.CutPrefix(line, "/boot/vmlinuz-"); ok && k != "" {
			return k, nil
		}
	}
	return "", fmt.Errorf("builder: %s carries no alpine /boot/vmlinuz-* — not an alpine ISO", iso)
}

// probeOverlayEntries is the overlay content shared by both carriers: the
// tar.gz apkovl (virtual media) and the appended initramfs cpio segment
// (network boot). With an overlay present, the initramfs skips its default
// boot services (init: `-f .default_boot_services -o ! -f "$ovl"`) — the
// marker restores sysinit/boot/shutdown so hardware, modloop and console
// services run.
func probeOverlayEntries(script string) []cpioEntry {
	return []cpioEntry{
		{Name: "etc/.default_boot_services", Mode: 0o100644, Body: []byte{}},
		{Name: "etc/local.d/", Mode: 0o040755},
		{Name: "etc/local.d/mammoth-probe.start", Mode: 0o100755, Body: []byte(script)},
		{Name: "etc/runlevels/default/local", Mode: 0o120777, Link: "/etc/init.d/local"},
	}
}

// probeOverlay builds the apkovl tar.gz: the probe script under
// etc/local.d/ plus the runlevel symlink that makes openrc execute it.
func probeOverlay(reportURL, staticCIDR, staticGateway, kernel string) ([]byte, error) {
	script := probeScript(reportURL, staticCIDR, staticGateway, kernel)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, f := range probeOverlayEntries(script) {
		hdr := &tar.Header{Name: f.Name, Mode: f.Mode}
		switch {
		case strings.HasSuffix(f.Name, "/"):
			hdr.Typeflag = tar.TypeDir
		case f.Link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = f.Link
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if f.Body != nil && f.Link == "" {
			if _, err := tw.Write(f.Body); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// probeScript is the busybox-sh probe: scan /sys, bring up NICs one by one
// (udhcpc) and POST the layout JSON, then power the machine off. A failed
// report drops to a shell instead of hanging silently — the BMC SOL session
// stays diagnosable. The JSON matches the inband_ssh snapshot shape
// (docs/04-install-spec.md §3; end_bytes = start + size − 1).
func probeScript(reportURL, staticCIDR, staticGateway, kernel string) string {
	return `#!/bin/sh
# mammoth ramdisk probe — /sys scan + report + poweroff (docs/05-inventory.md §4)
URL="` + reportURL + `"
STATIC_CIDR="` + staticCIDR + `"
STATIC_GW="` + staticGateway + `"
KERN="` + kernel + `"
OUT=/tmp/probe.json

log() { echo "[mammoth-probe] $*" > /dev/console; }

json_escape() {
	printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

scan() {
	printf '{"captured_at":"%s","source":"ramdisk","disks":[' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$OUT"
	first=1
	for d in /sys/block/*; do
		name="${d##*/}"
		case "$name" in loop*|ram*|sr*|fd*|dm-*|md*) continue ;; esac
		[ -r "$d/size" ] || continue
		sect=$(cat "$d/size") || continue
		[ "$sect" -gt 0 ] 2>/dev/null || continue
		model=$(json_escape "$(cat "$d/device/model" 2>/dev/null | tr -s ' \t' ' ' | sed 's/^ //; s/ $//')")
		serial=$(json_escape "$(cat "$d/device/serial" 2>/dev/null)")
		line="\"device\":\"$name\",\"size_bytes\":$((sect * 512))"
		[ -n "$model" ]  && line="$line,\"model\":\"$model\""
		[ -n "$serial" ] && line="$line,\"serial\":\"$serial\""
		line="$line,\"partitions\":["
		pfirst=1
		for p in "$d"/"$name"[0-9]* "$d"/"$name"p[0-9]*; do
			[ -r "$p/start" ] || continue
			pn="${p##*/}"
			case "$pn" in
				"$name"[0-9]*) num="${pn#"$name"}" ;;
				"$name"p[0-9]*) num="${pn#"$name"p}" ;;
				*) continue ;;
			esac
			pstart=$(( $(cat "$p/start") * 512 ))
			psize=$(( $(cat "$p/size") * 512 ))
			[ $pfirst -eq 1 ] && pfirst=0 || line="$line,"
			line="$line{\"number\":$num,\"start_bytes\":$pstart,\"end_bytes\":$((pstart + psize - 1)),\"size_bytes\":$psize}"
		done
		line="$line]"
		[ $first -eq 1 ] && first=0 || printf ',' >> "$OUT"
		printf '{%s}' "$line" >> "$OUT"
	done
	printf ']}' >> "$OUT"
}

try_post() {
	for i in 1 2 3; do
		if wget -q -T 10 -O /dev/null --post-file="$OUT" "$URL"; then
			log "report delivered via $1"
			return 0
		fi
		log "report attempt $i via $1 failed"
		sleep 2
	done
	return 1
}

report() {
	for nic in /sys/class/net/*; do
		n="${nic##*/}"
		[ "$n" = "lo" ] && continue
		log "probing via $n (dhcp)"
		ip link set "$n" up 2>/dev/null
		udhcpc -i "$n" -n -q -t 6 -T 3 >/dev/null 2>&1
		try_post "$n" && return 0
	done
	# No DHCP answered: apply the fallback address; the gateway is only
	# needed when the report URL is in a different subnet.
	if [ -n "$STATIC_CIDR" ]; then
		for nic in /sys/class/net/*; do
			n="${nic##*/}"
			[ "$n" = "lo" ] && continue
			log "static fallback on $n ($STATIC_CIDR)"
			ip addr add "$STATIC_CIDR" dev "$n" 2>/dev/null
			ip link set "$n" up 2>/dev/null
			if [ -n "$STATIC_GW" ]; then
				ip route replace default via "$STATIC_GW" dev "$n" 2>/dev/null
			fi
			try_post "$n" && return 0
		done
	fi
	return 1
}

log "probe start (kernel $KERN)"
scan
log "scan done: $(wc -c < "$OUT") bytes"
if report; then
	poweroff -f
fi
log "report failed — dropping to shell for diagnosis"
exec /bin/sh
`
}
