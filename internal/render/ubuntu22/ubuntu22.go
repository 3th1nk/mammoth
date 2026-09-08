// Package ubuntu22 implements the OSDriver for Ubuntu Server autoinstall
// (subiquity) — the second supported distro (docs/06-install-pipeline.md §5).
// Mechanics: the installer fetches a nocloud seed (meta-data + user-data)
// from Mammoth via `ds=nocloud-net;s=<task-token URL>/`; network stanzas use
// netplan (cloud-init network-config v2) whose `match.macaddress` is the
// batch-stable selector natively — no pre-install resolution needed.
// Keep-partition support is partial (docs/06-install-pipeline.md §5 matrix):
// keep: disk works via curtin storage config; keep: partitions is rejected.
package ubuntu22

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// Driver is the ubuntu22 autoinstall driver.
type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Distro() string { return "ubuntu22" }
func (d *Driver) SupportedArchs() []render.Arch {
	return []render.Arch{render.ArchAMD64, render.ArchARM64}
}

// KeepPartitionSupport: subiquity/curtin can keep a whole disk (skip it in
// the storage config) but block-level partition reuse needs curtin surgery —
// declared partial; keep: partitions is rejected at submit and at render.
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportPartial }

// RenderAnswers produces the nocloud seed files and boot parameters.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("ubuntu22: answer/completion URLs are required")
	}
	primaryURL := strings.TrimSuffix(in.AnswerBaseURL, "/") + "/user-data"
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("ubuntu22: image source is required")
	}

	storage, err := storageConfig(in, m)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	network := netplanConfig(in.Network)

	// late-commands: user post_install scripts run in-target first, then the
	// completion report unblocks the install stage (mirrors the kickstart
	// %post contract, docs/06-install-pipeline.md §4).
	var late []string
	late = append(late, "echo mammoth-install-finished")
	for _, s := range in.Scripts {
		if s.Stage != "post_install" {
			continue
		}
		late = append(late, scriptLine(s))
	}
	late = append(late, fmt.Sprintf("curl -fsS -m 10 -X POST -H 'Content-Type: application/json' --data-binary '{\"status\":\"ok\",\"detail\":\"autoinstall finished\"}' %s", in.CompleteURL))

	// pre_install scripts map to early-commands (installer environment).
	var early []string
	for _, s := range in.Scripts {
		if s.Stage == "pre_install" {
			early = append(early, scriptLine(s))
		}
	}

	auto := map[string]any{
		"version": 1,
		"locale":  "en_US.UTF-8",
		"keyboard": map[string]any{
			"layout": "us",
		},
		"ssh": map[string]any{
			"install-server":  true,
			"allow-pw":        true,
			"authorized-keys": in.SSHPublicKeys,
		},
		"storage": storage,
		"late-commands": append([]string{
			fmt.Sprintf("curtin in-target -- hostnamectl set-hostname %s", in.Hostname),
		}, late...),
	}
	if len(early) > 0 {
		auto["early-commands"] = early
	}
	if len(network) > 0 {
		auto["network"] = network
	}

	userData := map[string]any{
		"autoinstall": auto,
	}
	// Access: root password (per-task random when generate) rides in the
	// seed like the kickstart rootpw; ssh keys go through autoinstall.ssh.
	if in.RootPassword != "" {
		userData["chpasswd"] = map[string]any{
			"expire": false,
			"users": []map[string]string{
				{"name": "root", "password": in.RootPassword, "type": "text"},
			},
		}
	}

	meta := "" // meta-data content is unused by nocloud-net; the file must exist

	userDataYAML, err := emitYAML(userData)
	if err != nil {
		return nil, render.BootParams{}, fmt.Errorf("ubuntu22: user-data: %w", err)
	}

	answers := []render.AnswerFile{
		{Name: "meta-data", Content: meta},
		{Name: "user-data", Content: userDataYAML},
	}
	seedURL := strings.TrimSuffix(primaryURL, "/user-data")
	boot := render.BootParams{
		AnswerURL:  primaryURL,
		KernelArgs: fmt.Sprintf("autoinstall ds=nocloud-net;s=%s/", seedURL),
	}
	return answers, boot, nil
}

// storageConfig builds the curtin storage config (autoinstall.storage).
// Wiped disks get the full disk→partition→format→mount chain; kept disks are
// simply absent (curtin never touches them) — the partial keep semantics.
func storageConfig(in render.InstallInputs, m render.MachineView) (map[string]any, error) {
	config := []map[string]any{}
	for _, disk := range in.Disks {
		if disk.KeepDisk {
			continue
		}
		if len(disk.Baseline) > 0 || len(disk.Remove) > 0 || hasPreserve(disk.Partitions) {
			return nil, fmt.Errorf(
				"ubuntu22: keep: partitions is not supported on this distro (partial); submit without preserve")
		}
		if !disk.Wipe {
			return nil, fmt.Errorf("ubuntu22: disk %s must declare wipe or keep", disk.Device)
		}

		diskID := "disk-" + disk.Device
		// Hardware size bounds the "rest" partition (curtin needs a number).
		var diskBytes int64
		if m.Hardware != nil {
			for _, hd := range m.Hardware.Disks {
				if hd.Name == disk.Device {
					diskBytes = hd.SizeBytes
				}
			}
		}

		entry := map[string]any{
			"id":          diskID,
			"type":        "disk",
			"path":        "/dev/" + disk.Device,
			"ptable":      "gpt",
			"wipe":        "superblock",
			"name":        "mammoth-" + disk.Device,
			"grub_device": true,
		}
		config = append(config, entry)

		// Pre-compute grow sizes: rest takes the remainder minus a 1MiB gap
		// per partition (alignment headroom).
		const gap = int64(1024 * 1024)
		var fixed int64
		growCount := 0
		for _, p := range disk.Partitions {
			if p.Grow {
				growCount++
			} else {
				fixed += int64(p.SizeMB) * 1024 * 1024
			}
		}
		var restSize int64
		if growCount > 0 && diskBytes > 0 {
			restSize = diskBytes - fixed - gap*int64(len(disk.Partitions))
			if restSize < gap {
				restSize = gap
			}
		}

		for i, p := range disk.Partitions {
			num := i + 1
			partID := fmt.Sprintf("part-%s-%d", disk.Device, num)
			part := map[string]any{
				"id":     partID,
				"type":   "partition",
				"device": diskID,
				"number": num,
				"size":   fmt.Sprintf("%dM", p.SizeMB),
				"wipe":   "superblock",
			}
			if hasFlag(p.Flags, "esp") {
				part["flag"] = "boot"
			}
			if p.Grow {
				part["size"] = restSize
			}
			config = append(config, part)

			fstype := p.FS
			if hasFlag(p.Flags, "esp") {
				fstype = "fat32"
			}
			fID := fmt.Sprintf("fmt-%s-%d", disk.Device, num)
			config = append(config, map[string]any{
				"id":     fID,
				"type":   "format",
				"volume": partID,
				"fstype": fstype,
			})
			if p.Mount != "" && p.Mount != "swap" {
				config = append(config, map[string]any{
					"id":     fmt.Sprintf("mnt-%s-%d", disk.Device, num),
					"type":   "mount",
					"path":   p.Mount,
					"device": fID,
				})
			}
		}
	}
	if len(config) == 0 {
		return nil, fmt.Errorf("ubuntu22: storage config is empty")
	}
	return map[string]any{"version": 1, "config": config}, nil
}

// netplanConfig renders cloud-init network-config v2 (netplan semantics).
// match.macaddress is the batch-stable selector — subiquity applies it
// natively, no pre-install resolution required (docs/04-install-spec.md §5.2).
func netplanConfig(entries []render.NetworkEntry) map[string]any {
	if len(entries) == 0 {
		return nil
	}
	out := map[string]any{"version": 2}
	ethernets := map[string]any{}
	bonds := map[string]any{}
	applyAddrs := func(m map[string]any, e render.NetworkEntry) {
		if len(e.Addresses) > 0 {
			m["addresses"] = e.Addresses
		}
		var gw string
		var routes []map[string]any
		for _, r := range e.Routes {
			if r.To == "default" {
				gw = r.Via
				continue
			}
			routes = append(routes, map[string]any{"to": r.To, "via": r.Via})
		}
		if gw != "" {
			m["gateway4"] = gw
		}
		if len(routes) > 0 {
			m["routes"] = routes
		}
		if len(e.Nameservers) > 0 {
			ns := map[string]any{"addresses": e.Nameservers}
			if len(e.Search) > 0 {
				ns["search"] = e.Search
			}
			m["nameservers"] = ns
		}
		if e.MTU > 0 {
			m["mtu"] = e.MTU
		}
	}

	for i, e := range entries {
		switch {
		case e.Bond != nil:
			var ids []string
			for _, mac := range e.Bond.SlavesMACs {
				id := fmt.Sprintf("if-%s-%d", strings.ToLower(strings.ReplaceAll(mac, ":", "")), i)
				eth := map[string]any{
					"match": map[string]any{"macaddress": strings.ToLower(mac)},
				}
				if e.MTU > 0 {
					eth["mtu"] = e.MTU
				}
				ethernets[id] = eth
				ids = append(ids, id)
			}
			params := map[string]any{
				"mode": e.Bond.Mode,
			}
			// netplan parameter names: miimon → mii-monitor-interval etc.
			for k, v := range e.Bond.Params {
				params[k] = v
			}
			bond := map[string]any{
				"interfaces": ids,
				"parameters": params,
			}
			applyAddrs(bond, e)
			bonds[fmt.Sprintf("bond%d", i)] = bond
		case e.VLAN != nil:
			vlans := map[string]any{}
			v := map[string]any{"id": e.VLAN.ID, "link": e.VLAN.Link}
			applyAddrs(v, e)
			vlans[fmt.Sprintf("vlan%d", e.VLAN.ID)] = v
			out["vlans"] = vlans
		default:
			if e.Match == nil || (e.Match.MAC == "" && e.Match.Name == "") {
				continue // render-level guard: caller validated earlier
			}
			id := e.Match.Name
			eth := map[string]any{}
			if e.Match.MAC != "" {
				eth["match"] = map[string]any{"macaddress": strings.ToLower(e.Match.MAC)}
			}
			if e.SetName != "" {
				eth["set-name"] = e.SetName
			}
			if len(e.Addresses) == 0 {
				eth["dhcp4"] = true
			} else {
				eth["dhcp4"] = false
				applyAddrs(eth, e)
			}
			key := id
			if key == "" {
				key = fmt.Sprintf("if-%s", strings.ToLower(strings.ReplaceAll(e.Match.MAC, ":", "")))
			}
			ethernets[key] = eth
		}
	}
	if len(ethernets) > 0 {
		out["ethernets"] = ethernets
	}
	if len(bonds) > 0 {
		out["bonds"] = bonds
	}
	return out
}

func scriptLine(s render.ScriptEntry) string {
	if s.Inline != "" {
		return "curtin in-target -- sh -c " + quoteSh(s.Inline)
	}
	if s.URL != "" {
		return "curtin in-target -- sh -c " + quoteSh("curl -fsS "+s.URL+" | sh")
	}
	return ""
}

func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// emitYAML renders the seed as JSON: cloud-init accepts JSON user-data
// (any JSON value after #cloud-config), so no YAML dependency is needed and
// the output is deterministic for golden tests.
func emitYAML(v map[string]any) (string, error) {
	b := &strings.Builder{}
	b.WriteString("#cloud-config\n")
	enc := json.NewEncoder(b)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func hasPreserve(parts []render.ResolvedPartition) bool {
	for _, p := range parts {
		if p.Preserve {
			return true
		}
	}
	return false
}
