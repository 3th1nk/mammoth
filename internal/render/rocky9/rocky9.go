// Package rocky9 implements the OSDriver for RHEL-lineage kickstart
// (Rocky/Alma) — the first supported distro (docs/06-install-pipeline.md §5,
// support matrix). Anaconda mechanics: the answer file is fetched from the
// Mammoth server via inst.ks; network stanzas are generated at install time
// by a %pre hook that resolves MAC → interface name (batch-stable selection),
// so bonds and static addresses survive the installer's device naming.
package rocky9

import (
	"fmt"
	"net/netip"
	"strings"
	"text/template"

	"github.com/3th1nk/mammoth/internal/render"
)

// Driver is the rocky9 kickstart driver.
type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Distro() string { return "rocky9" }
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
var ksTemplate = template.Must(template.New("ks").Parse(`# Mammoth — task {{.TaskToken}} / machine {{.MachineID}}
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

url --url={{.ImageSource}}
bootloader --location=mbr{{if .BootDrive}} --boot-drive={{.BootDrive}}{{end}}
{{- if .WipeDrives}}
zerombr
clearpart --drives={{.WipeDrives}} --initlabel --all
{{- end}}
{{- range .PartLines}}
{{.}}
{{- end}}

%packages
@core
openssh-server
curl
%end

%pre --erroronfail
set -e
{{- range .PreScripts}}
{{.}}
{{- end}}
%end

%post --erroronfail
set -e
{{- range .PostScripts}}
{{.}}
{{- end}}
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"status":"ok","detail":"kickstart %post finished"}' {{.CompleteURL}}
%end
`))

// networkShell emits the sh snippet executed in %pre: resolve MAC → interface
// name (batch-stable selector), then produce network stanzas — static, bond,
// vlan — using the resolved names (docs/04-install-spec.md §5.2: mac is the
// primary selector; Mammoth never allocates addresses).
func networkShell(entries []render.NetworkEntry) (string, error) {
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
	return b.String(), nil
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
		if r.To == "default" {
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
	if in.AnswerURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("rocky9: answer/completion URLs are required")
	}
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("rocky9: image source is required")
	}

	var wipe []string
	var partLines []string
	for _, disk := range in.Disks {
		if disk.KeepDisk {
			continue // untouched (M4 handles preserve semantics end to end)
		}
		if !disk.Wipe {
			return nil, render.BootParams{}, fmt.Errorf("rocky9: disk %s without wipe or keep is unsupported until M4", disk.Device)
		}
		wipe = append(wipe, disk.Device)
		for _, p := range disk.Partitions {
			if p.Preserve {
				return nil, render.BootParams{}, fmt.Errorf("rocky9: preserve partitions arrive with M4")
			}
			fs := p.FS
			if hasFlag(p.Flags, "esp") {
				fs = "efi"
			}
			line := fmt.Sprintf("part %s --fstype=%s --ondisk=%s", p.Mount, fs, disk.Device)
			switch {
			case p.Grow:
				line += " --grow"
			case p.SizeMB > 0:
				line += fmt.Sprintf(" --size=%d", p.SizeMB)
			default:
				return nil, render.BootParams{}, fmt.Errorf("rocky9: partition %s on %s needs a size or rest", p.Mount, disk.Device)
			}
			if p.Mount == "swap" {
				line = strings.Replace(line, "part swap", "part swap", 1)
			}
			partLines = append(partLines, line)
		}
	}

	var preScripts, postScripts []string
	for _, s := range in.Scripts {
		switch s.Stage {
		case "pre_install":
			preScripts = append(preScripts, scriptBody(s))
		case "post_install":
			postScripts = append(postScripts, scriptBody(s))
		default:
			return nil, render.BootParams{}, fmt.Errorf("rocky9: unknown script stage %q", s.Stage)
		}
	}

	netPre := ""
	if len(in.Network) > 0 {
		shell, err := networkShell(in.Network)
		if err != nil {
			return nil, render.BootParams{}, err
		}
		netPre = shell
	}

	data := map[string]any{
		"TaskToken":     in.TaskToken,
		"MachineID":     in.MachineID,
		"Hostname":      in.Hostname,
		"RootPassword":  in.RootPassword,
		"SSHPublicKeys": in.SSHPublicKeys,
		"ImageSource":   in.ImageSource,
		"BootDrive":     in.BootDrive,
		"WipeDrives":    strings.Join(wipe, ","),
		"PartLines":     partLines,
		"NetworkPre":    netPre != "",
		"NetworkShell":  netPre,
		"PreScripts":    preScripts,
		"PostScripts":   postScripts,
		"CompleteURL":   in.CompleteURL,
	}
	if in.Hostname != "" {
		// kickstart sets hostname via the network command or a %post; the
		// static form is a %post (works for both static and dhcp installs).
		postScripts = append([]string{
			"hostnamectl set-hostname " + in.Hostname + " || echo " + in.Hostname + " > /etc/hostname",
		}, postScripts...)
		data["PostScripts"] = postScripts
	}

	var buf strings.Builder
	if err := ksTemplate.Execute(&buf, data); err != nil {
		return nil, render.BootParams{}, fmt.Errorf("rocky9: template: %w", err)
	}

	answers := []render.AnswerFile{{Name: "ks.cfg", Content: buf.String()}}
	boot := render.BootParams{
		AnswerURL:  in.AnswerURL,
		KernelArgs: fmt.Sprintf("inst.ks=%s inst.repo=cdrom inst.text", in.AnswerURL),
	}
	return answers, boot, nil
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
