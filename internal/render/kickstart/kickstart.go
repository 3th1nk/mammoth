// Package kickstart implements the OSDriver for RHEL-lineage anaconda
// kickstart — Rocky/Alma (the first supported distro) and UOS Server V20
// (docs/06-install-pipeline.md §5, support matrix). Anaconda mechanics: the answer file is fetched from the
// Mammoth server via inst.ks; network stanzas are generated at install time
// by a %pre hook that resolves MAC → interface name (batch-stable selection),
// so bonds and static addresses survive the installer's device naming.
package kickstart

import (
	"fmt"
	"net/netip"
	"path"
	"strings"
	"text/template"

	"github.com/3th1nk/mammoth/internal/render"
)

// dynDisk is one claimed disk that must be re-identified in %pre: ph is the
// $Dn placeholder its stanzas reference; device/sizeBytes/serial drive the
// %pre resolution (docs/compat/huawei.md — Redfish logical drive names differ
// from installer device names).
type dynDisk struct {
	idx       int
	ph        string
	device    string
	sizeBytes int64
	serial    string
	partLines []string
}

// Driver is the rocky9 kickstart driver. One instance per distro name —
// the anaconda/kickstart dialect also covers UOS Server V20 (anaconda-based,
// RHEL-style install tree with AppStream/BaseOS), registered as a separate
// distro name.
type Driver struct {
	distro string
}

// New returns the driver for one distro name ("rocky9", "uniontechos").
func New(distro string) *Driver { return &Driver{distro: distro} }

func (d *Driver) Distro() string {
	if d.distro == "" {
		return "rocky9" // keeps the zero-value constructor usable in tests
	}
	return d.distro
}

// dialectExtras covers installer deltas between distro members of the same
// kickstart package. UOS Server's anaconda (33.16 UOS build) crashes in its
// Finish phase with "max() arg is an empty sequence" under a fully
// unattended kickstart — its Finish task groups expect an EULA ack and a
// created user. Both commands are harmless no-ops on standard RHEL-lineage
// anaconda (and the eula command is skipped with a warning where absent).
func (d *Driver) dialectExtras() string {
	switch d.distro {
	case "uniontechos":
		return "eula --agreed\nuser --name=uos --password=Uos@2024 --plaintext --groups=wheel"
	}
	return ""
}
func (d *Driver) SupportedArchs() []render.Arch {
	return []render.Arch{render.ArchAMD64, render.ArchARM64}
}

// KeepPartitionSupport: RHEL-lineage has the most complete keep mechanism
// (%pre + --onpart/--noformat) per the support matrix. The M4 milestone
// wires the %pre drift machinery; M3 rejects keep at submit time.
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportFull }

// ksTemplate is the kickstart dialect. Dynamic pieces:
//   - network: %pre resolves MAC→iface names and writes an include file
//     (bond slaves reference MACs — names are not stable across distros);
//   - storage: resolved disks/partitions from verify_layout;
//   - %post: user scripts, then the completion callback that unblocks the
//     install stage (docs/06-install-pipeline.md §3, §4).
//
// failtrap renders the per-hook ERR trap: a failing %pre/%post reports its
// phase to the completion endpoint, so the task error carries the failing
// installer phase instead of an opaque timeout — the portable stand-in for
// console capture (the drift guard keeps its own precise LAYOUT_DRIFT report).
func failtrap(detail, completeURL string) string {
	payload := fmt.Sprintf(`{\"status\":\"failed\",\"detail\":\"%s\"}`, detail)
	return fmt.Sprintf(
		`trap 'curl -m 5 -sS -X POST -H "Content-Type: application/json" -d "%s" %s >/dev/null 2>&1 || true' ERR`,
		payload, completeURL)
}

var ksTemplate = template.Must(template.New("ks").Funcs(template.FuncMap{
	"failtrap": failtrap,
}).Parse(`# Mammoth — task {{.TaskToken}} / machine {{.MachineID}}
# Rendered by the mammoth server; fetched via inst.ks over the task-token URL.
text
reboot
lang en_US.UTF-8
keyboard us
timezone UTC
{{- if .RootPassword}}
rootpw --plaintext {{.RootPassword}}
{{- else}}
rootpw --lock
{{- end}}
selinux --permissive
firstboot --disable
services --enabled=sshd
{{- range .SSHPublicKeys}}
sshkey --username=root "{{.}}"
{{- end}}
{{- if .NetworkPre}}

%pre --erroronfail
set -e
{{failtrap "network pre_install failed" .CompleteURL}}
mkdir -p /run/install/mammoth
cat > /run/install/mammoth/network.sh <<'MAMMOTH_NET'
{{.NetworkShell}}
MAMMOTH_NET
sh /run/install/mammoth/network.sh > /run/install/mammoth/90-network.ks
%end
%include /run/install/mammoth/90-network.ks
{{- else}}

network --bootproto=dhcp --activate
{{- end}}
{{- if .Hostname}}

network --hostname={{.Hostname}}
{{- end}}
{{- if .StoragePre}}

%pre --erroronfail
set -e
{{failtrap "storage pre_install failed" .CompleteURL}}
mkdir -p /run/install/mammoth
cat > /run/install/mammoth/storage.sh <<'MAMMOTH_STORE'
{{.StorageShell}}
MAMMOTH_STORE
sh /run/install/mammoth/storage.sh > /run/install/mammoth/90-storage.ks
%end
%include /run/install/mammoth/90-storage.ks
{{- end}}

{{.RepoCmd}}
{{.DialectExtras}}
{{- if not .StoragePre}}
bootloader{{if .BootDrive}} --boot-drive={{.BootDrive}}{{end}}
{{- if .WipeDrives}}
zerombr
clearpart --drives={{.WipeDrives}} --initlabel --all
{{- end}}
{{- end}}
{{- if .RemoveParts}}
clearpart --list={{.RemoveParts}}
{{- end}}
{{- range .PartLines}}
{{.}}
{{- end}}
{{- range .RaidMemberLines}}
{{.}}
{{- end}}
{{- range .RaidLines}}
{{.}}
{{- end}}

%packages
@core
openssh-server
curl
%end

%post --nochroot --erroronfail
set -e
{{.GrowRootScript}}
%end

%pre --erroronfail
set -e
{{failtrap "pre_install script failed" .CompleteURL}}
{{- range .PreScripts}}
{{.}}
{{- end}}
%end
{{- if .DriftScript}}

%pre --erroronfail
set -e
{{.DriftScript}}
%end
{{- end}}

%post --erroronfail
set -e
{{failtrap "post_install script failed" .CompleteURL}}
{{- range .PostScripts}}
{{.}}
{{- end}}
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"status":"ok","detail":"kickstart %post finished"}' {{.CompleteURL}}
%end
`))

// hasExt4RootGrow reports whether the layout declares an ext4 root with
// grow semantics (the shape that needs the post-install extension).
func hasExt4RootGrow(disks []render.ResolvedDisk) bool {
	for _, d := range disks {
		if d.KeepDisk {
			continue
		}
		for _, p := range d.Partitions {
			if p.Mount == "/" && p.Grow && p.FS == "ext4" {
				return true
			}
		}
	}
	return false
}

// growRootScript extends the root partition to disk end and grows the
// filesystem. blivet clamps --grow at the 2^32 sector boundary on controller
// volumes (real-hardware: 3.6T disk, root stopped at 2TiB) — anaconda also
// ignores --maxsize there, so the extension happens in %post instead.
//
// The %post runs with --nochroot, against anaconda's target mount
// /mnt/sysimage. parted refuses to resizepart a mounted partition (script
// mode answers its warning with No), so the extension uses sfdisk — the same
// mechanism as cloud-utils-growpart — followed by an online resize2fs.
// Failure-tolerant: the install is unaffected.
func growRootScript() string {
	return `root_src=$(findmnt -nro SOURCE /mnt/sysimage) && root_disk=$(lsblk -nro PKNAME "$root_src") && root_num=${root_src##*[a-z]} || exit 0
echo ", +" | sfdisk --no-reread --force -N "$root_num" "/dev/$root_disk"
partprobe "/dev/$root_disk" 2>/dev/null || partx -u "/dev/$root_disk"
resize2fs "$root_src" || echo "grow root extension skipped (non-fatal)"`
}

// networkShell emits the sh snippet executed in %pre: resolve MAC → interface
// name (batch-stable selector), then produce network stanzas — static, bond,
// vlan — using the resolved names (docs/04-install-spec.md §5.2: mac is the
// primary selector; Mammoth never allocates addresses).
func networkShell(entries []render.NetworkEntry, hostname string) (string, error) {
	var b strings.Builder
	b.WriteString("# MAC-resolved network stanzas, produced by mammoth\n")
	b.WriteString("iface_by_mac() { for d in /sys/class/net/*; do [ \"$(cat \"$d/address\")\" = \"$1\" ] && basename \"$d\" && return 0; done; return 1; }\n")
	for i, e := range entries {
		switch {
		case e.Bond != nil:
			if len(e.Addresses) == 0 {
				return "", fmt.Errorf("network[%d]: bond requires addresses", i)
			}
			addr, err := cidrSplit(e.Addresses[0])
			if err != nil {
				return "", fmt.Errorf("network[%d]: %w", i, err)
			}
			var slaves []string
			for _, mac := range e.Bond.SlavesMACs {
				slaves = append(slaves, fmt.Sprintf("$(iface_by_mac %s)", strings.ToLower(mac)))
			}
			opts := []string{"mode=" + e.Bond.Mode}
			for k, v := range e.Bond.Params {
				opts = append(opts, k+"="+v)
			}
			fmt.Fprintf(&b, "bond_slaves=$(IFS=,; echo \"%s\")\n", strings.Join(slaves, ",")) //nolint:govet
			fmt.Fprintf(&b, "echo \"network --device=bond0 --bondslaves=$bond_slaves --bondopts=%s --bootproto=static --ip=%s --netmask=%s",
				strings.Join(opts, ","), addr.ip, addr.mask)
			gw := defaultVia(e.Routes)
			if gw != "" {
				fmt.Fprintf(&b, " --gateway=%s", gw)
			}
			for _, ns := range e.Nameservers {
				fmt.Fprintf(&b, " --nameserver=%s", ns)
			}
			if e.MTU > 0 {
				fmt.Fprintf(&b, " --mtu=%d", e.MTU)
			}
			b.WriteString(" --activate\"\n")
		case e.VLAN != nil:
			addr, err := cidrSplit(first(e.Addresses))
			if err != nil {
				return "", fmt.Errorf("network[%d]: %w", i, err)
			}
			fmt.Fprintf(&b, "echo \"network --device=%s.%d --bootproto=static --ip=%s --netmask=%s --activate\"\n",
				e.VLAN.Link, e.VLAN.ID, addr.ip, addr.mask)
		default:
			if e.Match == nil || (e.Match.MAC == "" && e.Match.Name == "") {
				return "", fmt.Errorf("network[%d]: match (mac preferred) required", i)
			}
			selector := e.Match.Name
			shellRef := selector
			if e.Match.MAC != "" {
				shellRef = fmt.Sprintf("$(iface_by_mac %s)", strings.ToLower(e.Match.MAC))
				_ = selector
			}
			if len(e.Addresses) == 0 {
				fmt.Fprintf(&b, "echo \"network --device=%s --bootproto=dhcp --activate\"\n", shellRef)
				continue
			}
			addr, err := cidrSplit(e.Addresses[0])
			if err != nil {
				return "", fmt.Errorf("network[%d]: %w", i, err)
			}
			fmt.Fprintf(&b, "echo \"network --device=%s --bootproto=static --ip=%s --netmask=%s", shellRef, addr.ip, addr.mask)
			gw := defaultVia(e.Routes)
			if gw != "" {
				fmt.Fprintf(&b, " --gateway=%s", gw)
			}
			for _, ns := range e.Nameservers {
				fmt.Fprintf(&b, " --nameserver=%s", ns)
			}
			if e.SetName != "" {
				fmt.Fprintf(&b, " --interfacename=%s", e.SetName)
			}
			if e.MTU > 0 {
				fmt.Fprintf(&b, " --mtu=%d", e.MTU)
			}
			b.WriteString(" --activate\"\n")
		}
	}
	out := b.String()
	// hostname rides the FIRST full network stanza: a bare
	// `network --hostname=` line without a device is not applied by
	// anaconda on every kickstart path (real-hardware: the host came up
	// with its default name while the stanza sat in the ks file).
	if hostname != "" {
		if i := strings.Index(out, " --activate\""); i >= 0 {
			out = out[:i] + " --hostname=" + hostname + out[i:]
		}
	}
	return out, nil
}

type ipmask struct{ ip, mask string }

// cidrSplit converts a CIDR address into kickstart's ip+netmask pair
// (conversion happens at render time; %pre sh must not depend on ipcalc).
func cidrSplit(cidr string) (ipmask, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return ipmask{}, fmt.Errorf("invalid address %q: %w", cidr, err)
	}
	mask := prefixToMask(p.Bits())
	if mask == "" {
		return ipmask{}, fmt.Errorf("unsupported prefix length /%d", p.Bits())
	}
	return ipmask{ip: p.Addr().String(), mask: mask}, nil
}

func prefixToMask(bits int) string {
	if bits < 0 || bits > 32 {
		return ""
	}
	var m uint32
	for i := 0; i < bits; i++ {
		m |= 1 << (31 - i)
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
}

func defaultVia(routes []render.NetRoute) string {
	for _, r := range routes {
		if r.To == "default" || r.To == "0.0.0.0/0" || r.To == "::/0" {
			return r.Via
		}
	}
	return ""
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// RenderAnswers produces the kickstart and boot parameters.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: answer/completion URLs are required", d.distro)
	}
	primaryURL := strings.TrimSuffix(in.AnswerBaseURL, "/") + "/ks.cfg"
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: image source is required", d.distro)
	}

	var wipe []string
	var removeList []string
	var partLines []string
	var raidMemberLines []string
	var raidLines []string
	var drift string
	// Disks claimed under non-kernel names (Redfish logical drives report
	// e.g. "LogicalDrive1"; the installer sees sda/sdb) are resolved in %pre
	// by size+serial: their part lines carry $Dn placeholders expanded by
	// storage.sh into a dynamic %include (same pattern as network.sh).
	var dyn []dynDisk
	var dynMemberLines []string
	dynPH := func(device string) (string, bool) {
		for _, d := range dyn {
			if d.device == device {
				return d.ph, true
			}
		}
		return "", false
	}
	for _, disk := range in.Disks {
		switch {
		case disk.KeepDisk:
			// keep: disk — never touched: no clearpart, no part lines.
			continue
		case len(disk.Baseline) > 0 || len(disk.Remove) > 0 || hasPreserve(disk.Partitions):
			// keep: partitions — preserved partitions are reused
			// unformatted (--onpart + --noformat, original UUID); the
			// snapshot's non-preserved partitions are removed precisely
			// (clearpart --list), leaving the rest of the disk alone.
			// Device names here come from in-band snapshots — kernel names.
			removeList = append(removeList, disk.Remove...)
			if in.DriftCheck && len(disk.Baseline) > 0 {
				drift += driftGuardScript(disk.Device, disk.Baseline, in.CompleteURL)
			}
			for _, p := range disk.Partitions {
				if p.Preserve {
					if p.OnPart == "" {
						return nil, render.BootParams{}, fmt.Errorf(
							"%s: preserved partition %s has no onpart binding", d.distro, p.Mount)
					}
					partLines = append(partLines,
						fmt.Sprintf("part %s --onpart=%s --noformat", p.Mount, p.OnPart))
					continue
				}
				line, err := d.newPartLine(p, disk.Device, disk.SizeBytes)
				if err != nil {
					return nil, render.BootParams{}, err
				}
				partLines = append(partLines, line)
			}
		default:
			if !disk.Wipe {
				return nil, render.BootParams{}, fmt.Errorf(
					"%s: disk %s must declare wipe or keep", d.distro, disk.Device)
			}
			if render.IsKernelDeviceName(disk.Device) {
				wipe = append(wipe, disk.Device)
				for _, p := range disk.Partitions {
					line, err := d.newPartLine(p, disk.Device, disk.SizeBytes)
					if err != nil {
						return nil, render.BootParams{}, err
					}
					partLines = append(partLines, line)
				}
				continue
			}
			// Redfish-claimed name: resolve in %pre, emit dynamic lines.
			dd := dynDisk{idx: len(dyn), ph: fmt.Sprintf("$D%d", len(dyn)),
				device: disk.Device, sizeBytes: disk.SizeBytes, serial: disk.Serial}
			for _, p := range disk.Partitions {
				line, err := d.newPartLine(p, dd.ph, dd.sizeBytes)
				if err != nil {
					return nil, render.BootParams{}, err
				}
				dd.partLines = append(dd.partLines, line)
			}
			dyn = append(dyn, dd)
		}
	}

	// hardware raid volumes bind to their discovered drives: fresh logical
	// drives get clearpart (harmless) and normal part lines. Controller
	// assigned names (LogicalDriveN) are not kernel names — those resolve in
	// %pre by size, like disks[].
	for _, r := range in.Raid {
		if r.Mode != "hardware" || r.BoundDevice == "" {
			continue
		}
		if render.IsKernelDeviceName(r.BoundDevice) {
			wipe = append(wipe, r.BoundDevice)
			for _, p := range r.Partitions {
				line, err := d.newPartLine(p, r.BoundDevice, r.SizeBytes)
				if err != nil {
					return nil, render.BootParams{}, err
				}
				partLines = append(partLines, line)
			}
			continue
		}
		dd := dynDisk{idx: len(dyn), ph: fmt.Sprintf("$D%d", len(dyn)),
			device: r.BoundDevice, sizeBytes: r.SizeBytes}
		for _, p := range r.Partitions {
			line, err := d.newPartLine(p, dd.ph, dd.sizeBytes)
			if err != nil {
				return nil, render.BootParams{}, err
			}
			dd.partLines = append(dd.partLines, line)
		}
		dyn = append(dyn, dd)
	}

	// software raid: per-member full-disk raid partitions + one md per volume.
	// Constraint (documented): one mount per volume — anaconda raid lines map
	// one md to one mount; multi-partition volumes need LVM (later).
	for _, r := range in.Raid {
		if r.Mode != "software" {
			continue
		}
		if len(r.Partitions) != 1 {
			return nil, render.BootParams{}, fmt.Errorf(
				"%s: software raid volume %s supports exactly one partition (LVM arrives later)", d.distro, r.Name)
		}
		var tags []string
		for mi, member := range r.Members {
			tag := fmt.Sprintf("raid.%s-%d", r.Name, mi)
			// Members claimed under Redfish names resolve via the same $Dn
			// placeholders as disks[] (verify_layout consumed the same pool);
			// their part lines must live in the dynamic include where the
			// placeholders actually expand.
			if ph, ok := dynPH(member); ok {
				dynMemberLines = append(dynMemberLines,
					fmt.Sprintf("part %s --size=1 --grow --ondisk=%s", tag, ph))
			} else {
				raidMemberLines = append(raidMemberLines,
					fmt.Sprintf("part %s --size=1 --grow --ondisk=%s", tag, member))
			}
			tags = append(tags, tag)
		}
		p := r.Partitions[0]
		raidLines = append(raidLines, fmt.Sprintf("raid %s --fstype=%s --level=%s --device=%s %s",
			p.Mount, p.FS, r.Level, r.Name, strings.Join(tags, " ")))
	}

	var preScripts, postScripts []string
	for _, s := range in.Scripts {
		switch s.Stage {
		case "pre_install":
			preScripts = append(preScripts, scriptBody(s))
		case "post_install":
			postScripts = append(postScripts, scriptBody(s))
		default:
			return nil, render.BootParams{}, fmt.Errorf("%s: unknown script stage %q", d.distro, s.Stage)
		}
	}
	netPre := ""
	if len(in.Network) > 0 {
		shell, err := networkShell(in.Network, in.Hostname)
		if err != nil {
			return nil, render.BootParams{}, err
		}
		netPre = shell
	}

	// Dynamic storage resolution: when any claimed disk carries a non-kernel
	// name (Redfish logical drives), the whole wipe/clearpart set moves into
	// a %pre-generated %include so every name resolves at install time.
	storageShellText := ""
	bootDrive := in.BootDrive
	if len(dyn) > 0 {
		storageShellText = storageShell(dyn, wipe, dynMemberLines, bootDrive)
		bootDrive = "" // the bootloader line moves into the include
		wipe = nil     // clearpart is emitted by the include for all disks
	}

	// kickstart's `url` command speaks only http/https/ftp — an NFS-hosted
	// ISO installs via the `nfs` command (dir = the file's directory;
	// anaconda scans it for the ISO) or inst.repo on the kernel command line.
	repoCmd := "url --url=" + in.ImageSource
	if strings.HasPrefix(in.ImageSource, "nfs://") {
		u := strings.TrimPrefix(in.ImageSource, "nfs://")
		if i := strings.Index(u, "/"); i > 0 {
			repoCmd = fmt.Sprintf("nfs --server=%s --dir=%s", u[:i], path.Dir(u[i:]))
		}
	}

	data := map[string]any{
		"TaskToken":       in.TaskToken,
		"MachineID":       in.MachineID,
		"Hostname":        in.Hostname,
		"RootPassword":    in.RootPassword,
		"SSHPublicKeys":   in.SSHPublicKeys,
		"ImageSource":     in.ImageSource,
		"BootDrive":       bootDrive,
		"WipeDrives":      strings.Join(wipe, ","),
		"RemoveParts":     strings.Join(removeList, ","),
		"PartLines":       partLines,
		"RaidMemberLines": raidMemberLines,
		"RaidLines":       raidLines,
		"DriftScript":     drift,
		"NetworkPre":      netPre != "",
		"NetworkShell":    netPre,
		"StoragePre":      storageShellText != "",
		"RepoCmd":         repoCmd,
		"DialectExtras":   d.dialectExtras(),
		"StorageShell":    storageShellText,
		"PreScripts":      preScripts,
		"PostScripts":     postScripts,
		"CompleteURL":     in.CompleteURL,
		// blivet clamps --grow at the 2^32 sector boundary on controller
		// volumes (real-hardware: 3.6T disk, root stopped at 2TiB) — a
		// %post --nochroot extends the root partition to disk end after
		// anaconda's own (clamped) allocation.
		"GrowRootExtension": hasExt4RootGrow(in.Disks),
		"GrowRootScript":    growRootScript(),
	}
	if in.Hostname != "" {
		// kickstart sets hostname via the network command or a %post; the
		// static form is a %post (works for both static and dhcp installs).
		// hostnamectl cannot reach systemd from the %post chroot (it either
		// fails or no-ops) — write /etc/hostname directly; the native
		// `network --hostname=` command above is the primary mechanism.
		postScripts = append([]string{
			"echo " + in.Hostname + " > /etc/hostname",
		}, postScripts...)
		data["PostScripts"] = postScripts
	}

	var buf strings.Builder
	if err := ksTemplate.Execute(&buf, data); err != nil {
		return nil, render.BootParams{}, fmt.Errorf("%s: template: %w", d.distro, err)
	}

	// inst.repo: when the distro source is an NFS-hosted ISO, anaconda can
	// fetch packages from it directly — this allows single-slot BMCs to boot
	// with ONLY the boot ISO (packages come over the network).
	repo := "cdrom"
	if strings.HasPrefix(in.ImageSource, "nfs://") {
		// anaconda nfs syntax requires host:/path (an .iso path is loop-mounted
		// automatically — the supported way to install from an NFS-hosted ISO;
		// an HTTP ISO file is NOT installable: anaconda can only fetch an
		// unpacked tree over HTTP).
		u := strings.TrimPrefix(in.ImageSource, "nfs://")
		if i := strings.Index(u, "/"); i > 0 {
			u = u[:i] + ":" + u[i:]
		}
		repo = "nfs:" + u
	} else {
		repo = in.ImageSource
	}

	answers := []render.AnswerFile{{Name: "ks.cfg", Content: buf.String()}}
	// Early network (dracut ip=/ifname=): the answer file is a remote URL on
	// the boot media's kernel command line — anaconda must have network up
	// BEFORE it can fetch it, and without an ip= argument it configures none
	// (real-hardware finding: the installer sat idle and never fetched the
	// kickstart). Spec-defined static interfaces become pinned dracut args
	// (ifname= by MAC, so they work regardless of in-installer NIC naming);
	// DHCP is the fallback. The kickstart's own network stanzas apply
	// afterwards.
	early := render.EarlyNetArgs(in.Network, true)
	if early == "" {
		early = "ip=dhcp"
	}
	boot := render.BootParams{
		AnswerURL:           primaryURL,
		KernelArgs:          fmt.Sprintf("%s inst.ks=%s inst.repo=%s inst.text", early, primaryURL, repo),
		InstallerAutoReboot: true, // kickstart's reboot command
	}
	return answers, boot, nil
}

// storageShell emits the sh snippet executed in %pre: claimed disks are
// re-identified by size (+serial when known) among the installer's block
// devices, and the wipe/part/bootloader stanzas land in a dynamic %include.
// The tolerance is 1% (min 64MiB) — Redfish and kernel capacities agree to
// within controller rounding, and real disks never sit that close.
func storageShell(dyn []dynDisk, wipeStatic []string, dynMemberLines []string, bootDrive string) string {
	var b strings.Builder
	b.WriteString("# size/serial-resolved storage stanzas, produced by mammoth\n")
	b.WriteString("resolve() {\n")
	b.WriteString("  want_size=$1; want_serial=$2; excl=$3; best=\"\"; bestdiff=0\n")
	b.WriteString("  while read -r name size serial type; do\n")
	b.WriteString("    [ \"$type\" = \"disk\" ] || continue\n")
	b.WriteString("    case \" $excl \" in *\" $name \"*) continue ;; esac\n")
	b.WriteString("    if [ -n \"$want_serial\" ] && [ -n \"$serial\" ] && [ \"$serial\" != \"$want_serial\" ]; then continue; fi\n")
	b.WriteString("    diff=$((size - want_size)); [ $diff -lt 0 ] && diff=$((-diff))\n")
	b.WriteString("    if [ -z \"$best\" ] || [ $diff -lt $bestdiff ]; then best=$name; bestdiff=$diff; fi\n")
	b.WriteString("  done <<MAMMOTH_DISKS\n")
	b.WriteString("$(lsblk -dnb -o NAME,SIZE,SERIAL,TYPE)\n")
	b.WriteString("MAMMOTH_DISKS\n")
	b.WriteString("  tol=$((want_size / 100)); [ $tol -lt 67108864 ] && tol=67108864\n")
	b.WriteString("  if [ -z \"$best\" ] || [ $bestdiff -gt $tol ]; then\n")
	b.WriteString("    echo \"mammoth: no disk for size=$want_size serial=$want_serial (closest $best off by $bestdiff)\" >&2\n")
	b.WriteString("    exit 1\n")
	b.WriteString("  fi\n")
	b.WriteString("  echo \"$best\"\n")
	b.WriteString("}\n")

	excl := ""
	for _, d := range dyn {
		fmt.Fprintf(&b, "D%d=$(resolve %d '%s' '%s')\n", d.idx, d.sizeBytes, d.serial, excl)
		excl += "$D" + fmt.Sprint(d.idx) + " "
	}
	if len(wipeStatic) == 0 && len(dyn) == 0 {
		return b.String() // nothing to write (defensive; callers skip empty)
	}
	b.WriteString("mkdir -p /run/install/mammoth\n")
	b.WriteString("cat > /run/install/mammoth/90-storage.ks <<MAMMOTH_STORAGE_KS\n")
	b.WriteString("zerombr\n")
	drives := make([]string, 0, len(dyn)+len(wipeStatic))
	for _, d := range dyn {
		drives = append(drives, "$D"+fmt.Sprint(d.idx))
	}
	drives = append(drives, wipeStatic...)
	fmt.Fprintf(&b, "clearpart --drives=%s --initlabel --all\n", strings.Join(drives, ","))
	for _, d := range dyn {
		for _, line := range d.partLines {
			b.WriteString(line + "\n")
		}
	}
	for _, line := range dynMemberLines {
		b.WriteString(line + "\n")
	}
	if bootDrive != "" {
		ph := bootDrive
		for _, d := range dyn {
			if d.device == bootDrive {
				ph = "$D" + fmt.Sprint(d.idx)
			}
		}
		fmt.Fprintf(&b, "bootloader --boot-drive=%s\n", ph)
	} else {
		b.WriteString("bootloader\n")
	}
	b.WriteString("MAMMOTH_STORAGE_KS\n")
	return b.String()
}

func scriptBody(s render.ScriptEntry) string {
	if s.Inline != "" {
		return s.Inline
	}
	if s.URL != "" {
		return fmt.Sprintf("curl -fsS %s | sh", s.URL)
	}
	return ""
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func hasPreserve(parts []render.ResolvedPartition) bool {
	for _, p := range parts {
		if p.Preserve {
			return true
		}
	}
	return false
}

// newPartLine renders a fresh (non-preserved) partition line.
func (d *Driver) newPartLine(p render.ResolvedPartition, device string, diskSizeBytes int64) (string, error) {
	fs := p.FS
	if hasFlag(p.Flags, "esp") {
		fs = "efi"
	}
	line := fmt.Sprintf("part %s --fstype=%s --ondisk=%s", p.Mount, fs, device)
	switch {
	case p.Grow:
		line += " --grow"
		// blivet's grow allocation can clamp at the 2^32 sector boundary on
		// controller volumes (real-hardware: 3.6T disk, root stopped at
		// 2TiB) — an explicit maxsize (disk capacity in MB) removes the
		// ambiguity; anaconda grows to min(maxsize, available tail).
		if diskSizeBytes > 0 {
			line += fmt.Sprintf(" --maxsize=%d", diskSizeBytes/1048576)
		}
	case p.SizeMB > 0:
		line += fmt.Sprintf(" --size=%d", p.SizeMB)
	default:
		return "", fmt.Errorf("%s: partition %s on %s needs a size or rest", d.distro, p.Mount, device)
	}
	return line, nil
}

// driftGuardScript emits the %pre layout drift guard: per preserved
// partition, compare the live table (sysfs start/size sectors + blkid UUID)
// against the verify_layout-bound snapshot baseline; any drift reports
// LAYOUT_DRIFT to the completion endpoint and aborts the install
// (docs/06-install-pipeline.md §4 — "装错盘在机制上不可能发生").
func driftGuardScript(device string, baseline []render.BaselinePartition, completeURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth layout drift guard — %s, baseline bound at verify_layout\n", device)
	b.WriteString("report() { curl -fsS -m 10 -X POST -H 'Content-Type: application/json' ")
	b.WriteString(`--data-binary '{"status":"failed","detail":"LAYOUT_DRIFT: '`)
	b.WriteString(`"$1"`)
	b.WriteString(`'"}' `)
	b.WriteString(completeURL)
	b.WriteString(" >/dev/null 2>&1 || true; echo \"LAYOUT_DRIFT: $1\" >&2; exit 1; }\n")
	const sector = int64(512)
	for _, bp := range baseline {
		fmt.Fprintf(&b, "\n# preserve %s (number %d)\n", bp.Device, bp.Number)
		fmt.Fprintf(&b, "_d=/sys/block/%s/%s\n", device, bp.Device)
		fmt.Fprintf(&b, "[ -e \"$_d/start\" ] || report \"%s missing\"\n", bp.Device)
		if bp.StartBytes > 0 {
			fmt.Fprintf(&b, "[ \"$(cat $_d/start)\" = \"%d\" ] || report \"%s start drifted\"\n",
				bp.StartBytes/sector, bp.Device)
		}
		if bp.SizeBytes > 0 {
			fmt.Fprintf(&b, "[ \"$(cat $_d/size)\" = \"%d\" ] || report \"%s size drifted\"\n",
				bp.SizeBytes/sector, bp.Device)
		}
		if bp.UUID != "" {
			fmt.Fprintf(&b, "[ \"$(blkid -s UUID -o value /dev/%s)\" = \"%s\" ] || report \"%s uuid drifted\"\n",
				bp.Device, bp.UUID, bp.Device)
		}
	}
	return b.String()
}
