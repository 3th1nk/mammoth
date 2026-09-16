package preseed

import (
	"fmt"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// The scripts ride on the boot medium at /cdrom/run/mammoth/ and are invoked
// by preseed/early_command (installer environment) and preseed/late_command
// (installer environment with the fresh system at /target). The d-i
// environment has busybox wget but no curl, so the completion callback — the
// contract that unblocks the install stage — posts JSON with wget.
//
// failtrap mirrors the rocky9 %pre/%post ERR trap: a failing hook reports its
// phase to the completion endpoint, so the task error carries the failing
// installer phase instead of an opaque INSTALL_TIMEOUT.
func failtrap(phase, completeURL string) string {
	return fmt.Sprintf(
		"trap 'wget -q -T 5 -O /dev/null --post-data=\"{\\\"status\\\":\\\"failed\\\",\\\"detail\\\":\\\"%s failed\\\"}\" %s >/dev/null 2>&1 || true' EXIT",
		phase, completeURL)
}

// preInstallScript runs in the installer environment: user pre_install
// scripts execute before partitioning (the %pre contract), then the trap is
// cleared so the install itself is no longer guarded by this hook.
//
// A dynamic target (controller-named hardware-RAID volume) is resolved here
// first: the kernel device is matched by size among /sys/block entries and
// seeded into partman-auto/disk and grub-installer/bootdev via debconf-set —
// the official dynamic-preseed shape. A failed resolution exits non-zero and
// rides the failtrap: mammoth sees pre_install failed while partman, with no
// disk set, stalls instead of writing the wrong device.
func preInstallScript(in render.InstallInputs, t target) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Mammoth pre_install stage (installer environment; early_command).\n")
	b.WriteString(failtrap("pre_install", in.CompleteURL) + "\n")
	b.WriteString("set -e\n")
	if t.dynamic {
		fmt.Fprintf(&b, "%s", deviceResolveScript(t))
	}
	// The unpacked-ISO pool is an unsigned mirror: the distro ISO layout
	// (Debian 13+) ships a bare Release with no detached signature, and
	// apt-setup's mirror verification runs `apt-get update`, whose index
	// signature check allow_unauthenticated does NOT cover — the install
	// stalls in "failed to access the mirror" without these.
	b.WriteString("mkdir -p /etc/apt/apt.conf.d\n")
	b.WriteString("printf 'Acquire::AllowInsecureRepositories \"true\";\\nAPT::Get::AllowUnauthenticated \"true\";\\n' > /etc/apt/apt.conf.d/99mammoth-insecure\n")
	for _, s := range in.Scripts {
		if s.Stage != "pre_install" {
			continue
		}
		if s.Inline != "" {
			b.WriteString("# mammoth user pre_install script\n")
			b.WriteString("sh -c " + quoteSh(s.Inline) + "\n")
		}
	}
	b.WriteString("trap - EXIT\n")
	return b.String()
}

// deviceResolveScript re-identifies a hardware-RAID volume by capacity: the
// tolerance mirrors kickstart's %pre resolver (1%, floor 64MiB) — Redfish and
// kernel capacities agree to within controller rounding. busybox arithmetic
// is 64-bit; the size sysfs node counts 512-byte sectors.
func deviceResolveScript(t target) string {
	var b strings.Builder
	b.WriteString("# resolve the hardware-RAID volume (controller name -> kernel device, by size)\n")
	fmt.Fprintf(&b, "want=%d\n", t.sizeBytes)
	b.WriteString("best=\"\"; bestdiff=0\n")
	b.WriteString("for d in /sys/block/*; do\n")
	b.WriteString("  name=${d##*/}\n")
	b.WriteString("  case \"$name\" in loop*|ram*|dm-*|sr*|md*) continue ;; esac\n")
	b.WriteString("  [ -f \"$d/size\" ] || continue\n")
	b.WriteString("  size=$(($(cat \"$d/size\") * 512))\n")
	b.WriteString("  diff=$((size - want)); [ $diff -lt 0 ] && diff=$((-diff))\n")
	b.WriteString("  if [ -z \"$best\" ] || [ $diff -lt $bestdiff ]; then best=$name; bestdiff=$diff; fi\n")
	b.WriteString("done\n")
	b.WriteString("tol=$((want / 100)); [ $tol -lt 67108864 ] && tol=67108864\n")
	b.WriteString("if [ -z \"$best\" ] || [ $bestdiff -gt $tol ]; then\n")
	b.WriteString("  echo \"mammoth: no block device matches size=$want (closest $best off by $bestdiff)\" >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	fmt.Fprintf(&b, "echo \"mammoth: install target %s -> /dev/$best (off by $bestdiff bytes)\" >&2\n", t.device)
	b.WriteString("debconf-set partman-auto/disk /dev/$best\n")
	b.WriteString("debconf-set grub-installer/bootdev /dev/$best\n")
	return b.String()
}

// postInstallScript finalizes the fresh system at /target: ssh access
// (authorized keys + PermitRootLogin — the preseed installed openssh-server
// but d-i ships prohibit-password by default), user post_install scripts,
// then the completion report.
func postInstallScript(distro string, in render.InstallInputs) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Mammoth post_install stage (installer environment; /target is the new system).\n")
	b.WriteString(failtrap("post_install", in.CompleteURL) + "\n")
	b.WriteString("set -e\n")
	if in.Hostname != "" {
		// netcfg's hostname preseed does not always stick (dhcp-provided
		// names win, defaults reappear) — write the file directly, the
		// same contract as the ubuntu driver.
		b.WriteString("echo " + quoteSh(in.Hostname) + " > /target/etc/hostname\n")
		// netcfg's finish-install pass re-writes /etc/hostname AFTER
		// late_command (real-hardware: the echo above lost to a second
		// write and the host came up as "debian") — a first-boot oneshot
		// re-applies the name and removes itself.
		b.WriteString(`mkdir -p /target/etc/systemd/system/multi-user.target.wants
cat > /target/etc/systemd/system/mammoth-hostname.service <<'UNIT'
[Unit]
Description=Mammoth hostname provisioning
After=local-fs.target
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo ` + in.Hostname + ` > /etc/hostname; systemctl disable mammoth-hostname.service; rm -f /etc/systemd/system/mammoth-hostname.service /etc/systemd/system/multi-user.target.wants/mammoth-hostname.service'
[Install]
WantedBy=multi-user.target
UNIT
ln -sf /etc/systemd/system/mammoth-hostname.service /target/etc/systemd/system/multi-user.target.wants/mammoth-hostname.service
`)
	}
	if len(in.SSHPublicKeys) > 0 {
		b.WriteString("mkdir -p /target/root/.ssh\n")
		b.WriteString("chmod 700 /target/root/.ssh\n")
		for _, k := range in.SSHPublicKeys {
			b.WriteString("echo " + quoteSh(k) + " >> /target/root/.ssh/authorized_keys\n")
		}
		b.WriteString("chmod 600 /target/root/.ssh/authorized_keys\n")
	}
	b.WriteString("mkdir -p /target/etc/ssh/sshd_config.d\n")
	b.WriteString("echo 'PermitRootLogin yes' > /target/etc/ssh/sshd_config.d/60-mammoth.conf\n")
	for _, s := range in.Scripts {
		if s.Stage != "post_install" {
			continue
		}
		if s.Inline != "" {
			b.WriteString("# mammoth user post_install script\n")
			b.WriteString("in-target sh -c " + quoteSh(s.Inline) + "\n")
		}
		if s.URL != "" {
			// Fetch with the installer's wget (the target may have none),
			// execute inside the target, clean up.
			b.WriteString("wget -q -T 10 -O /target/tmp/mammoth-user-script.sh " + s.URL + "\n")
			b.WriteString("in-target sh /tmp/mammoth-user-script.sh\n")
			b.WriteString("rm -f /target/tmp/mammoth-user-script.sh\n")
		}
	}
	b.WriteString("trap - EXIT\n")
	fmt.Fprintf(&b, "wget -q -T 10 -O /dev/null --post-data='{\"status\":\"ok\",\"detail\":\"%s preseed install finished\"}' %s >/dev/null 2>&1\n", distro, in.CompleteURL)
	return b.String()
}

// quoteSh single-quotes a value for sh -c (the '"'"' idiom, same as the
// ubuntu22 driver).
func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
