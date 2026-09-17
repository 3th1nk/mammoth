// Package agent implements the OSDriver for the mammoth agent install path
// (docs/12-agent-initramfs.md): a distro installer never runs on the
// machine — the mammoth agent initramfs (an alpine disk-less root) reads a
// machine-readable plan rendered from the SAME declarative Install Spec the
// answer-file dialects consume, partitions the disks, and installs the base
// system from the package pool. The plan ships in two encodings of one
// dataset: agent-plan.json (the contract/documentation face) and
// agent-plan.sh (the executable face — busybox sh has no JSON parser, and
// every quote lives in shell rather than inside JSON strings, the same
// reason the preseed driver renders run/mammoth/*.sh as separate files).
//
// The first member of this dialect is "alpine": the agent installs the same
// distro the carrier ISO carries, from its /apks pool. Kickstart/preseed/
// autoinstall remain the compat path for distros whose installers must be
// preserved; this driver is where new-install-path conclusions are earned.
package agent

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// Driver is the mammoth-agent install driver. One instance per distro name
// ("alpine" today) — the Registry routes by Distro() and one instance
// carries one name.
type Driver struct {
	distro string
}

// New returns the driver for one distro name.
func New(distro string) *Driver { return &Driver{distro: distro} }

func (d *Driver) Distro() string                { return d.distro }
func (d *Driver) SupportedArchs() []render.Arch { return []render.Arch{render.ArchAMD64} }

// KeepPartitionSupport: the pilot agent rebuilds every target disk from the
// declared table — block-level reuse (keep: partitions/preserve) is not
// implemented yet. Submit-time gates reject keep usage for this driver.
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportNone }

// PXESupport: full — the alpine netboot carrier is the probe's proven
// payload path (tarball + modloop + apkovl over HTTP).
func (d *Driver) PXESupport() render.SupportLevel { return render.SupportFull }

// NetbootCarrier: the alpine NETBOOT tarball (shared with the ramdisk
// probe); the distro ISO contributes its /apks package repository to the
// boot tree.
func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierAlpineNetboot }

// NetbootPool: none — packages come from the boot-tree apks subtree, served
// under /netboot/files/<token>/apks; no shared content-addressed pool tree.
func (d *Driver) NetbootPool() render.NetbootPool { return render.NetbootPoolNone }

// AgentInstaller: the boot carrier must add the agent apkovl overlay to the
// boot media.
func (d *Driver) AgentInstaller() bool { return true }

// supportedFilesystems — everything else is rejected at render with a clear
// message (the agent's mkfs tooling comes from the pool; growing the set is
// a driver change plus a package-presence check, not a silent surprise).
var supportedFilesystems = map[string]bool{
	"ext4": true, "vfat": true, "swap": true,
}

// answerJSON and answerSH are the two plan encodings.
const (
	answerJSON  = "agent-plan.json"
	answerSH    = "agent-plan.sh"
	overlayName = "mammoth.apkovl.tar.gz"
)

// RenderAnswers produces the agent plan (JSON + sh) and the boot parameters.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: answer/completion URLs are required", d.distro)
	}
	if err := validate(in); err != nil {
		return nil, render.BootParams{}, fmt.Errorf("%s: %w", d.distro, err)
	}
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")
	json, err := d.renderJSON(in)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	sh := renderSH(in)
	return []render.AnswerFile{
			{Name: answerJSON, Content: json},
			{Name: answerSH, Content: sh},
		}, render.BootParams{
			AnswerURL:           base + "/" + answerSH,
			KernelArgs:          isoKernelArgs,
			InstallerAutoReboot: true,
			NetbootKernelArgs:   netbootKernelArgs(in),
		}, nil
}

// isoKernelArgs boots the agent from the repacked ISO: the overlay is
// auto-detected on the boot media root (apkovl= cannot carry a bare
// filename — probeiso.go finding), the plan is read from the same media, so
// the ISO carrier installs fully offline (the callback needs the network
// and is driven from the plan's URL).
const isoKernelArgs = "modules=loop,squashfs,sd-mod,usb-storage,ext4,vfat console=tty0 console=ttyS0,115200"

// netbootKernelArgs boots the agent over PXE: early DHCP, the network
// modloop, the HTTP apks repo (memory-root AND target package source), the
// apkovl overlay fetched by URL, and the plan base — the agent fetches
// agent-plan.sh from it before touching disks. The external base is derived
// from the answer URL ("<ext>/render/<token>"), the same file layout the
// boot tree publishes under /netboot/files/<token>/.
func netbootKernelArgs(in render.InstallInputs) string {
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")
	ext := strings.TrimSuffix(base, "/render/"+in.TaskToken)
	files := ext + "/netboot/files/" + in.TaskToken
	args := "modules=loop,squashfs,sd-mod,usb-storage console=tty0 console=ttyS0,115200 ip=dhcp"
	args += " modloop=" + files + "/modloop"
	args += " alpine_repo=" + files + "/apks"
	args += " apkovl=" + files + "/" + overlayNetbootName()
	args += " mammoth_base=" + base
	return args
}

// overlayNetbootName is the overlay file name in the per-task boot tree.
func overlayNetbootName() string { return "agent.apkovl.tar.gz" }

// validate rejects spec surfaces the pilot agent does not carry yet —
// explicitly, at render, instead of answering files that would fail on the
// machine hours later (the dialect gates: SCHEMA_* at submit covers keep
// usage via KeepPartitionSupport; these are the driver-internal ones).
func validate(in render.InstallInputs) error {
	if len(in.Raid) > 0 {
		return fmt.Errorf("declarative RAID is not supported by the agent path yet (pilot scope: plain disks)")
	}
	hasRoot := false
	for _, dsk := range in.Disks {
		for _, p := range dsk.Partitions {
			if p.Preserve {
				return fmt.Errorf("partition preservation is not supported by the agent path yet")
			}
			if !supportedFilesystems[strings.ToLower(p.FS)] {
				return fmt.Errorf("filesystem %q is not supported by the agent path yet (ext4/vfat/swap)", p.FS)
			}
			if p.Mount == "/" {
				hasRoot = true
			}
		}
	}
	for _, n := range in.Network {
		if n.Bond != nil {
			return fmt.Errorf("bonding is not supported by the agent path yet (pilot scope: plain interfaces)")
		}
		if n.VLAN != nil {
			return fmt.Errorf("vlan is not supported by the agent path yet (pilot scope: plain interfaces)")
		}
	}
	if !hasRoot {
		return fmt.Errorf("no declared partition mounts / (the agent needs a root filesystem target)")
	}
	return nil
}

// ── JSON plan (contract face) ───────────────────────────────────────────────

// planJSON mirrors the sh plan's dataset — the documented shape an agent
// implementation (current or future) can consume directly.
func (d *Driver) renderJSON(in render.InstallInputs) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n  \"version\": 1,\n  \"distro\": %q,\n  \"task_token\": %q,\n  \"machine_id\": %q,\n", d.distro, in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "  \"hostname\": %q,\n", in.Hostname)
	fmt.Fprintf(&b, "  \"root_password\": %q,\n", in.RootPassword)
	fmt.Fprintf(&b, "  \"boot_drive\": %q,\n", in.BootDrive)
	fmt.Fprintf(&b, "  \"complete_url\": %q,\n", in.CompleteURL)
	fmt.Fprintf(&b, "  \"packages\": [\"alpine-base\", \"linux-lts\", \"openssh\"],\n")
	fmt.Fprintf(&b, "  \"ssh_keys\": [")
	for i, k := range in.SSHPublicKeys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", strings.TrimSpace(k))
	}
	b.WriteString("],\n")

	b.WriteString("  \"disks\": [\n")
	for i, dsk := range in.Disks {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    {\"device\": %q, \"partitions\": [", dsk.Device)
		for j, p := range dsk.Partitions {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "{\"fs\": %q, \"mount\": %q, \"size_mb\": %d, \"grow\": %t, \"flags\": [%s]}",
				strings.ToLower(p.FS), p.Mount, p.SizeMB, p.Grow, flagsJSON(p.Flags))
		}
		b.WriteString("]}")
	}
	b.WriteString("\n  ],\n")

	b.WriteString("  \"network\": [\n")
	for i, n := range in.Network {
		if i > 0 {
			b.WriteString(",\n")
		}
		mac := ""
		if n.Match != nil {
			mac = n.Match.MAC
		}
		fmt.Fprintf(&b, "    {\"mac\": %q, \"addresses\": [%s], \"gateway\": %q, \"nameservers\": [%s]}",
			mac, cidrsJSON(n.Addresses), defaultGateway(n.Routes), strsJSON(n.Nameservers))
	}
	b.WriteString("\n  ],\n")

	b.WriteString("  \"scripts\": {")
	for i, s := range in.Scripts {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q: [{\"url\": %q, \"content_base64\": %q, \"expected_exit\": [%s]}]",
			s.Stage, s.URL, base64.StdEncoding.EncodeToString([]byte(s.Inline)), exitsJSON(s.ExpectedExit))
	}
	b.WriteString("}\n}\n")
	return b.String(), nil
}

func flagsJSON(flags []string) string {
	var parts []string
	for _, f := range flags {
		parts = append(parts, fmt.Sprintf("%q", f))
	}
	return strings.Join(parts, ", ")
}

func cidrsJSON(addrs []string) string {
	var parts []string
	for _, a := range addrs {
		parts = append(parts, fmt.Sprintf("%q", a))
	}
	return strings.Join(parts, ", ")
}

func strsJSON(list []string) string {
	var parts []string
	for _, s := range list {
		parts = append(parts, fmt.Sprintf("%q", s))
	}
	return strings.Join(parts, ", ")
}

func exitsJSON(list []int) string {
	var parts []string
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%d", e))
	}
	return strings.Join(parts, ", ")
}

// defaultGateway extracts the v4 default route's via (the declarative spec
// expresses the default gateway as a route to 0.0.0.0/0).
func defaultGateway(routes []render.NetRoute) string {
	for _, r := range routes {
		if r.To == "0.0.0.0/0" || r.To == "default" {
			return r.Via
		}
	}
	return ""
}

// ── shell plan (executable face) ────────────────────────────────────────────

// renderSH emits the data declarations the agent sources after defining the
// mammoth_* collector functions. One line per fact — grep-able, quote-safe,
// and readable in a BMC SOL session (the agent prints it before applying).
func renderSH(in render.InstallInputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth agent plan (executable face of agent-plan.json) — task %s / machine %s\n", in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "MAMMOTH_HOSTNAME=%s\n", shQuote(in.Hostname))
	fmt.Fprintf(&b, "MAMMOTH_ROOT_PASSWORD=%s\n", shQuote(in.RootPassword))
	fmt.Fprintf(&b, "MAMMOTH_BOOT_DRIVE=%s\n", shQuote(in.BootDrive))
	fmt.Fprintf(&b, "MAMMOTH_COMPLETE_URL=%s\n", shQuote(in.CompleteURL))
	fmt.Fprintf(&b, "MAMMOTH_PACKAGES=%s\n", shQuote("alpine-base linux-lts openssh"))
	fmt.Fprintf(&b, "MAMMOTH_SSH_KEYS=%s\n", shQuote(strings.Join(sshKeys(in.SSHPublicKeys), "\n")))

	b.WriteString("\n# disks: mammoth_disk <device>\n")
	for _, dsk := range in.Disks {
		fmt.Fprintf(&b, "mammoth_disk %s\n", shQuote(dsk.Device))
	}

	b.WriteString("\n# partitions: mammoth_partition <device> <mount> <fs> <size_mb|-> <flags>\n")
	for _, dsk := range in.Disks {
		for _, p := range dsk.Partitions {
			size := "-"
			if !p.Grow && p.SizeMB > 0 {
				size = fmt.Sprintf("%d", p.SizeMB)
			}
			fmt.Fprintf(&b, "mammoth_partition %s %s %s %s %s\n",
				shQuote(dsk.Device), shQuote(p.Mount), shQuote(strings.ToLower(p.FS)), shQuote(size), shQuote(strings.Join(p.Flags, ",")))
		}
	}

	b.WriteString("\n# network: mammoth_network <mac|-> <addresses|-> <gateway|-> <nameservers|->\n")
	for _, n := range in.Network {
		mac := "-"
		if n.Match != nil && n.Match.MAC != "" {
			mac = strings.ToLower(n.Match.MAC)
		}
		addrs := strings.Join(n.Addresses, ",")
		if addrs == "" {
			addrs = "-"
		}
		gw := defaultGateway(n.Routes)
		if gw == "" {
			gw = "-"
		}
		ns := strings.Join(n.Nameservers, ",")
		if ns == "" {
			ns = "-"
		}
		fmt.Fprintf(&b, "mammoth_network %s %s %s %s\n", shQuote(mac), shQuote(addrs), shQuote(gw), shQuote(ns))
	}

	b.WriteString("\n# scripts: mammoth_script <stage> <base64-content|-> <url|-> <expected-exits>\n")
	for _, s := range in.Scripts {
		content := "-"
		if s.Inline != "" {
			content = base64.StdEncoding.EncodeToString([]byte(s.Inline))
		}
		url := s.URL
		if url == "" {
			url = "-"
		}
		exits := "-"
		if len(s.ExpectedExit) > 0 {
			var parts []string
			for _, e := range s.ExpectedExit {
				parts = append(parts, fmt.Sprintf("%d", e))
			}
			exits = strings.Join(parts, ",")
		}
		fmt.Fprintf(&b, "mammoth_script %s %s %s %s\n", shQuote(s.Stage), shQuote(content), shQuote(url), shQuote(exits))
	}
	return b.String()
}

// sshKeys normalizes the declared keys (trimmed, ordered, de-duplicated).
func sshKeys(keys []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// shQuote single-quotes a value for busybox sh (the '\” escape is the only
// construct needed; every fact in the plan is a plain string).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
