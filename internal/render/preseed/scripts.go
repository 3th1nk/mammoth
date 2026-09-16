package preseed

import (
	"encoding/base64"
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

// preInstallScript runs in the installer environment (preseed/early_command,
// fired at preseed load): user pre_install scripts, apt trust config for the
// signed offline pool. NOT device resolution — at preseed-load time storage
// drivers have not run hw-detect yet (real-hardware: the LSI volume was
// absent from /sys/block 41s post-initrd, failing the resolve); that work
// belongs to partman/early_command (resolveDiskScript), which runs when
// partman starts with hardware enumerated.
func preInstallScript(in render.InstallInputs, t target) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Mammoth pre_install stage (installer environment; early_command).\n")
	b.WriteString(failtrap("pre_install", in.CompleteURL) + "\n")
	b.WriteString("set -e\n")
	// The unpacked-ISO pool is an unsigned mirror: the distro ISO layout
	// (Debian 13+) ships a bare Release with no detached signature, and
	// apt-setup's mirror verification runs `apt-get update`, whose index
	// signature check allow_unauthenticated does NOT cover — the install
	// stalls in "failed to access the mirror" without these.
	b.WriteString("mkdir -p /etc/apt/apt.conf.d\n")
	b.WriteString("printf 'Acquire::AllowInsecureRepositories \"true\";\\nAPT::Get::AllowUnauthenticated \"true\";\\n' > /etc/apt/apt.conf.d/99mammoth-insecure\n")

	// The pool's own signing key (the pool Release is re-signed by mammoth;
	// see builder.StageNetbootPool): both apt trustdb locations get it —
	// trusted.gpg.d is the documented one, the append to trusted.gpg covers
	// installer environments whose apt only consults the legacy keyring.
	if pub := netbootPoolKey(in); len(pub) > 0 {
		b.WriteString("mkdir -p /etc/apt/trusted.gpg.d\n")
		b.WriteString("base64 -d > /etc/apt/trusted.gpg.d/mammoth-pool.gpg <<'MAMMOTH_POOL_KEY'\n")
		b.WriteString(base64Encode(pub))
		b.WriteString("\nMAMMOTH_POOL_KEY\n")
		b.WriteString("cat /etc/apt/trusted.gpg.d/mammoth-pool.gpg >> /etc/apt/trusted.gpg\n")
	}
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

// resolveDiskScript resolves a dynamic target (controller-named
// hardware-RAID volume) at partman/early_command time: hw-detect has loaded
// the storage drivers, so the volume is visible under /sys/block. It matches
// by size, seeds partman-auto/disk and grub-installer/bootdev via
// debconf-set, and rides the same failtrap semantics — a failed resolution
// reports pre_install failed (with the block-device inventory in the detail,
// the eyes we had missing on real hardware) instead of letting partman write
// the wrong device. Runs over HTTP like the other hooks (the route serves
// run/mammoth/* multi-segment names).
func resolveDiskScript(in render.InstallInputs, t target) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Mammoth disk resolution (partman/early_command; storage enumerated).\n")
	b.WriteString(failtrap("pre_install", in.CompleteURL) + "\n")
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "want=%d\n", t.sizeBytes)
	b.WriteString("best=\"\"; bestdiff=0\n")
	b.WriteString("inv=\"\"\n")
	b.WriteString("for d in /sys/block/*; do\n")
	b.WriteString("  name=${d##*/}\n")
	b.WriteString("  case \"$name\" in loop*|ram*|dm-*|sr*|md*) continue ;; esac\n")
	b.WriteString("  [ -f \"$d/size\" ] || continue\n")
	b.WriteString("  size=$(($(cat \"$d/size\") * 512))\n")
	b.WriteString("  inv=\"$inv $name=$size\"\n")
	b.WriteString("  diff=$((size - want)); [ $diff -lt 0 ] && diff=$((-diff))\n")
	b.WriteString("  if [ -z \"$best\" ] || [ $diff -lt $bestdiff ]; then best=$name; bestdiff=$diff; fi\n")
	b.WriteString("done\n")
	b.WriteString("tol=$((want / 100)); [ $tol -lt 67108864 ] && tol=67108864\n")
	b.WriteString("if [ -z \"$best\" ] || [ $bestdiff -gt $tol ]; then\n")
	b.WriteString("  echo \"mammoth: no block device matches size=$want (inventory:$inv)\" >&2\n")
	b.WriteString("  wget -q -T 5 -O /dev/null --post-data=\"{\\\"status\\\":\\\"failed\\\",\\\"detail\\\":\\\"pre_install: no device matches size=$want (inventory:$inv)\\\"}\" " + in.CompleteURL + " >/dev/null 2>&1 || true\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	fmt.Fprintf(&b, "echo \"mammoth: install target %s -> /dev/$best (off by $bestdiff bytes)\" >&2\n", t.device)
	b.WriteString("debconf-set partman-auto/disk /dev/$best\n")
	b.WriteString("debconf-set grub-installer/bootdev /dev/$best\n")
	b.WriteString("trap - EXIT\n")
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
	// The key deb's offline-apt tolerances existed for apt-setup's in-target
	// mirror verification only; the provisioned system keeps the pool key
	// (mammoth's signing identity) but never a permanently permissive apt.
	b.WriteString("rm -f /target/etc/apt/apt.conf.d/99mammoth-offline\n")
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

// netbootPoolKey nil-safely reads the pool signing public key.
func netbootPoolKey(in render.InstallInputs) []byte {
	if in.Netboot == nil {
		return nil
	}
	return in.Netboot.PoolPublicKey
}

// base64Encode wraps the standard encoding for heredoc payloads (76-column
// wrapping keeps the generated script readable; busybox base64 -d accepts it).
func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
