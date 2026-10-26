// Package inbandssh implements the partition-level probe (docs/05-inventory.md §3):
// agent-less, read-only, one-shot. A single SSH connection runs a short
// read-only command set (lsblk / blkid / ip / sysfs offsets); nothing is
// written to the target machine and no state is changed — failure means
// failure, with no residue.
//
// Capability boundary (docs/05-inventory.md §3): when the machine is
// unreachable in-band, partition-level layout is physically unobtainable —
// no out-of-band path can read a partition table. Failures surface as
// classified errors (NETWORK_UNREACHABLE / CREDENTIAL_AUTH_FAILED /
// LAYOUT_PARSE_FAILED), never as a hanging task.
package inbandssh

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Credentials for the SSH session (from the ssh-type credential store).
type Credentials struct {
	Username string
	Password string
	// PrivateKey (PEM) enables public-key auth — the recommended in-band
	// path: provisioned systems commonly set PermitRootLogin
	// prohibit-password (Rocky 9 default), which refuses password auth for
	// root while accepting keys (real-hardware finding).
	PrivateKey string
}

// Runner executes the read-only command set over a transport. The SSH
// implementation is the production Runner; tests substitute a fake serving
// recorded outputs, which pins the "matches a manual lsblk" acceptance.
type Runner interface {
	Run(ctx context.Context, addr string, cred Credentials, script string) ([]byte, error)
}

// Script is the exact read-only command set. Single connection, bounded
// output, no side effects. The sysfs loop supplies partition start offsets,
// which lsblk does not expose but the layout contract requires
// (docs/04-install-spec.md §3: start_bytes/end_bytes). The ENV section
// reports known installer-runtime markers (/run/anaconda, /run/subiquity,
// …) — verify_ready uses it to tell the still-finishing installer apart
// from the freshly installed system (real-hardware: the installer env has
// its own sshd and can answer the post-install probe as a false positive).
const Script = `lsblk -J -b -o NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINT,PKNAME,PARTLABEL,PARTUUID,SERIAL,PTTYPE
echo '===BLKID==='
blkid -o export 2>/dev/null || true
echo '===LINK==='
ip -j link 2>/dev/null || true
echo '===PARTS==='
for f in /sys/block/*/*/start; do printf '%s ' "$f"; cat "$f"; done 2>/dev/null || true
echo '===BOND==='
for d in /sys/class/net/*; do
  [ -d "$d/bonding" ] || continue
  printf '%s|mode=%s|miimon=%s|slaves=%s|active=%s\n' "${d##*/}" "$(cat "$d/bonding/mode" 2>/dev/null)" "$(cat "$d/bonding/miimon" 2>/dev/null)" "$(cat "$d/bonding/slaves" 2>/dev/null)" "$(cat "$d/bonding/active_slave" 2>/dev/null)"
done 2>/dev/null || true
echo '===ENV==='
for m in /run/anaconda /run/subiquity /run/debian-installer /lib/debian-installer; do [ -e "$m" ] && echo "$m"; done 2>/dev/null || true`

// Sections split the combined output.
const (
	secLSBLK = "===BLKID==="
	secBLKID = "===LINK==="
	secLINK  = "===PARTS==="
	secBOND  = "===BOND==="
	secENV   = "===ENV==="
)

// Collector turns one Runner invocation into a layout snapshot plus refreshed
// NIC link info (docs/05-inventory.md §3: NIC facts refresh alongside layout).
type Collector struct {
	Runner Runner
}

// Result carries the collected facts.
type Result struct {
	Layout Layout
	NICs   []NICFacts
	// Bonds carries the live bond facts when the target ran a collector
	// with the bond section — nil when the section is absent (recorded
	// fixtures predating it): that reads as "facts unavailable", NOT as
	// "no bonds", and verify_ready degrades instead of failing on it.
	Bonds []BondFacts
	// InstallerEnv reports that the session landed in a running installer
	// environment (anaconda/subiquity/d-i markers present) rather than a
	// regular system. verify_ready refuses to verify against it; discovery
	// treats it as informational.
	InstallerEnv bool `json:"installer_env"`
}

// Layout is the snapshot contract shape (docs/04-install-spec.md §3).
type Layout struct {
	CapturedAt time.Time    `json:"captured_at"`
	Source     string       `json:"source"`
	Disks      []LayoutDisk `json:"disks"`
}

// LayoutDisk is one physical/RAID device with its partitions.
type LayoutDisk struct {
	Device     string            `json:"device"`
	Match      map[string]string `json:"match,omitempty"`
	Table      string            `json:"table,omitempty"`
	Partitions []LayoutPartition `json:"partitions"`
}

// LayoutPartition is one partition as the installer will see it.
type LayoutPartition struct {
	Number     int    `json:"number"`
	StartBytes int64  `json:"start_bytes"`
	EndBytes   int64  `json:"end_bytes"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	Label      string `json:"label,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	UUID       string `json:"uuid,omitempty"`
}

// NICFacts is the refreshed link info (MAC is the join key to hardware.nics).
type NICFacts struct {
	Name   string `json:"name"`
	MAC    string `json:"mac"`
	LinkUp bool   `json:"link_up"`
}

// BondFacts is the live bond state read from sysfs. MAC join happens via
// NICs — every bond member is also a link in the LINK section.
type BondFacts struct {
	Name   string   `json:"name"`
	Mode   string   `json:"mode,omitempty"` // kernel name (sysfs "802.3ad 4" → "802.3ad")
	MIImon int      `json:"miimon"`         // 0 = link monitoring off
	Slaves []string `json:"slaves"`         // member link names
	Active string   `json:"active,omitempty"`
}

// Error is the classified probe failure. Code uses the registry vocabulary:
// NETWORK_UNREACHABLE / CREDENTIAL_AUTH_FAILED / LAYOUT_PARSE_FAILED.
type Error struct {
	Code      string
	Detail    string
	Retryable bool
	Err       error
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

func (e *Error) Unwrap() error { return e.Err }

// Collect runs the command set once and parses all sections. The context
// bounds the whole operation (timeout isolation — docs/07-bmc.md §5).
func (c *Collector) Collect(ctx context.Context, addr string, cred Credentials) (*Result, error) {
	out, err := c.Runner.Run(ctx, addr, cred, Script)
	if err != nil {
		return nil, classifyRun(err)
	}
	res, perr := parse(string(out))
	if perr != nil {
		return nil, &Error{Code: "LAYOUT_PARSE_FAILED", Detail: perr.Error()}
	}
	res.Layout.CapturedAt = time.Now().UTC()
	res.Layout.Source = "inband_ssh"
	return res, nil
}

func classifyRun(err error) *Error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "auth failed"),
		strings.Contains(msg, "permission denied"):
		return &Error{Code: "CREDENTIAL_AUTH_FAILED", Detail: msg, Retryable: false, Err: err}
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "connection timed out"),
		strings.Contains(msg, "network is unreachable"), strings.Contains(msg, "context deadline exceeded"):
		return &Error{Code: "NETWORK_UNREACHABLE", Detail: msg, Retryable: true, Err: err}
	default:
		return &Error{Code: "NETWORK_UNREACHABLE", Detail: msg, Retryable: true, Err: err}
	}
}

// ── parsing ─────────────────────────────────────────────────────────────────

type lsblkDisk struct {
	Name       string       `json:"name"`
	Path       string       `json:"path"`
	Size       int64        `json:"size"`
	Type       string       `json:"type"`
	FSType     string       `json:"fstype"`
	Mountpoint *string      `json:"mountpoint"`
	Serial     string       `json:"serial"`
	PTType     string       `json:"pttype"`
	PartLabel  string       `json:"partlabel"`
	Children   []lsblkChild `json:"children"`
}

type lsblkChild struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	Size       int64   `json:"size"`
	Type       string  `json:"type"`
	FSType     string  `json:"fstype"`
	Mountpoint *string `json:"mountpoint"`
	PartLabel  string  `json:"partlabel"`
	PartUUID   string  `json:"partuuid"`
}

type blkidDev struct {
	Device string
	UUID   string
	Type   string
}

// parse converts the combined script output. Exported for tests.
func parse(out string) (*Result, error) {
	lsblkOut, blkidOut, linkOut, partsOut, bondOut, envOut, err := splitSections(out)
	if err != nil {
		return nil, err
	}
	disks, err := parseLSBLK(lsblkOut)
	if err != nil {
		return nil, err
	}
	blkid := parseBlkid(blkidOut)
	starts := parseStarts(partsOut)

	layout := Layout{Disks: []LayoutDisk{}}
	for _, d := range disks {
		if d.Type != "disk" && d.Type != "md" {
			continue // loop/rom/etc. are not install targets
		}
		ld := LayoutDisk{
			Device:     firstNonEmpty(d.Path, "/dev/"+d.Name),
			Partitions: []LayoutPartition{},
		}
		if d.Serial != "" {
			ld.Match = map[string]string{"serial": d.Serial}
		}
		ld.Table = tableType(d.PTType)

		for _, ch := range d.Children {
			if ch.Type != "part" {
				continue
			}
			p := LayoutPartition{
				Number:     partitionNumber(ch.Name, d.Name),
				StartBytes: starts[ch.Name],
				SizeBytes:  ch.Size,
				FSType:     firstNonEmpty(blkidFS(blkid, ch.Path), ch.FSType),
				Label:      ch.PartLabel,
				Mountpoint: deref(ch.Mountpoint),
			}
			p.EndBytes = p.StartBytes + ch.Size - 1
			if ch.Size <= 0 {
				p.EndBytes = p.StartBytes
			}
			if u := blkidUUID(blkid, ch.Path); u != "" {
				p.UUID = u
			} else {
				p.UUID = ch.PartUUID
			}
			ld.Partitions = append(ld.Partitions, p)
		}
		layout.Disks = append(layout.Disks, ld)
	}

	nics, err := parseLink(linkOut)
	if err != nil {
		// NIC facts are an enhancement; layout is the payload. Record the
		// parse outcome but keep the snapshot.
		nics = nil
	}
	return &Result{Layout: layout, NICs: nics, Bonds: parseBonds(bondOut), InstallerEnv: strings.TrimSpace(envOut) != ""}, nil
}

func splitSections(out string) (lsblk, blkid, link, parts, bond, env string, err error) {
	i1 := strings.Index(out, secLSBLK)
	i2 := strings.Index(out, secBLKID)
	i3 := strings.Index(out, secLINK)
	if i1 < 0 || i2 < 0 || i3 < 0 {
		return "", "", "", "", "", "", fmt.Errorf("output missing section markers (lsblk=%v blkid=%v link=%v)", i1 >= 0, i2 >= 0, i3 >= 0)
	}
	lsblk = out[:i1]
	blkid = out[i1+len(secLSBLK) : i2]
	link = out[i2+len(secBLKID) : i3]
	tail := out[i3+len(secLINK):]
	// The bond and installer-marker sections are optional: recorded fixtures
	// and older collected outputs may not carry them. Bond absence reads as
	// "facts unavailable" (nil, NOT "no bonds") — verify_ready degrades on
	// it; env absence reads as "not an installer".
	if i4 := strings.Index(tail, secBOND); i4 >= 0 {
		parts = tail[:i4]
		bond = tail[i4+len(secBOND):]
		if i5 := strings.Index(bond, secENV); i5 >= 0 {
			env = bond[i5+len(secENV):]
			bond = bond[:i5]
		}
	} else {
		parts = tail
		if i5 := strings.Index(tail, secENV); i5 >= 0 {
			env = tail[i5+len(secENV):]
			parts = tail[:i5]
		}
	}
	return lsblk, blkid, link, parts, bond, env, nil
}

// parseBonds reads the ===BOND=== section — one line per bond:
//
//	bond0|mode=802.3ad|miimon=100|slaves=ens1f0 ens1f1|active=ens1f0
//
// The sysfs mode reads as "<name> <number>"; the name is the compare key.
// Returns nil when the section is absent (recorded fixtures predating it),
// an empty (non-nil) slice when the machine simply has no bonds.
func parseBonds(out string) []BondFacts {
	if out == "" {
		return nil
	}
	bonds := []BondFacts{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, rest, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		f := BondFacts{Name: strings.TrimSpace(name), Slaves: []string{}}
		for _, kv := range strings.Split(rest, "|") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			switch k {
			case "mode":
				if fields := strings.Fields(v); len(fields) > 0 {
					f.Mode = fields[0]
				}
			case "miimon":
				f.MIImon, _ = strconv.Atoi(strings.TrimSpace(v))
			case "slaves":
				f.Slaves = strings.Fields(v)
			case "active":
				f.Active = strings.TrimSpace(v)
			}
		}
		bonds = append(bonds, f)
	}
	return bonds
}

func parseLSBLK(out string) ([]lsblkDisk, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, fmt.Errorf("lsblk produced no output")
	}
	var v struct {
		Blockdevices []lsblkDisk `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("lsblk json: %w", err)
	}
	return v.Blockdevices, nil
}

// parseBlkid reads `blkid -o export` blocks:
//
//	/dev/sda1: UUID="…" TYPE="vfat" PARTUUID="…" PARTLABEL="EFI"
func parseBlkid(out string) map[string]blkidDev {
	m := map[string]blkidDev{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, ":") {
			continue
		}
		dev, rest, _ := strings.Cut(line, ":")
		dev = strings.TrimSpace(dev)
		b := blkidDev{Device: dev}
		for _, kv := range splitKV(rest) {
			switch kv[0] {
			case "UUID":
				b.UUID = kv[1]
			case "TYPE":
				b.Type = kv[1]
			}
		}
		m[dev] = b
	}
	return m
}

func splitKV(s string) [][2]string {
	var out [][2]string
	for _, part := range strings.Split(s, " ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			out = append(out, [2]string{k, strings.Trim(v, `"`)})
		}
	}
	return out
}

func parseStarts(out string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		// /sys/block/nvme0n1/nvme0n1p1/start → nvme0n1p1
		segs := strings.Split(fields[0], "/")
		if len(segs) < 2 {
			continue
		}
		dev := segs[len(segs)-2]
		if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			m[dev] = v * 512 // sysfs reports 512-byte sectors
		}
	}
	return m
}

func parseLink(out string) ([]NICFacts, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var raw []struct {
		Ifname    string `json:"ifname"`
		Address   string `json:"address"`
		Operstate string `json:"operstate"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("ip link json: %w", err)
	}
	nics := make([]NICFacts, 0, len(raw))
	for _, r := range raw {
		if r.Ifname == "lo" {
			continue
		}
		nics = append(nics, NICFacts{
			Name:   r.Ifname,
			MAC:    r.Address,
			LinkUp: r.Operstate == "UP",
		})
	}
	return nics, nil
}

// partitionNumber derives the partition number from its name relative to the
// parent: nvme0n1p3 within nvme0n1 → 3; sda2 within sda → 2; md0p1 → 1.
func partitionNumber(name, parent string) int {
	rest := strings.TrimPrefix(name, parent)
	rest = strings.TrimPrefix(rest, "p")
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

// tableType normalizes sysfs/lsblk partition-table identifiers onto the
// contract vocabulary (docs/04-install-spec.md §3: gpt | mbr).
func tableType(pttype string) string {
	switch strings.ToLower(pttype) {
	case "dos", "mbr":
		return "mbr"
	case "gpt":
		return "gpt"
	default:
		return strings.ToLower(pttype)
	}
}

func blkidUUID(m map[string]blkidDev, dev string) string { return m[dev].UUID }
func blkidFS(m map[string]blkidDev, dev string) string   { return m[dev].Type }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
