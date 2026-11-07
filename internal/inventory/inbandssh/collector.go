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
	"path"
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
echo '===LVM==='
if command -v vgs >/dev/null 2>&1; then
  echo '---VGS---'
  vgs --noheadings --units b --separator , -o vg_name,pv_count,vg_size 2>/dev/null || true
  echo '---PVS---'
  pvs --noheadings --units b --separator , -o pv_name,vg_name,pv_size 2>/dev/null || true
  echo '---LVS---'
  lvs --noheadings --units b --separator , -o vg_name,lv_name,lv_size,lv_dm_path 2>/dev/null || true
fi
echo '===ENV==='
for m in /run/anaconda /run/subiquity /run/debian-installer /lib/debian-installer; do [ -e "$m" ] && echo "$m"; done 2>/dev/null || true
echo '===SYS==='
printf 'hostname=%s\n' "$(hostname 2>/dev/null || cat /etc/hostname 2>/dev/null || true)"
printf 'kernel=%s\n' "$(uname -r 2>/dev/null || true)"
printf 'cmdline=%s\n' "$(cat /proc/cmdline 2>/dev/null | tr '\n' ' ' | sed 's/ $//')"
printf 'timezone=%s\n' "$(timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone 2>/dev/null || true)"
printf 'os=%s\n' "$(grep PRETTY_NAME /etc/os-release 2>/dev/null | cut -d= -f2 | tr -d '"')"
printf 'uptime=%s\n' "$(cat /proc/uptime 2>/dev/null | cut -d' ' -f1)"`

// Sections split the combined output.
const (
	secLSBLK = "===BLKID==="
	secBLKID = "===LINK==="
	secLINK  = "===PARTS==="
	secBOND  = "===BOND==="
	secLVM   = "===LVM==="
	secENV   = "===ENV==="
	secSYS   = "===SYS==="
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
	// Lvm carries the live volume group facts when the target ran a
	// collector with the LVM section and lvm2 was present — nil otherwise
	// ("facts unavailable", NOT "no volume groups"; same degrade semantics
	// as Bonds). The same facts ride the Layout snapshot for keep:vg
	// bindings and the fresh-group name-collision precheck.
	Lvm []VgFacts
	// System carries the acceptance-report facts (hostname / kernel /
	// /proc/cmdline / timezone / os-release / uptime — the ===SYS=== section)
	// when the collector ran it. nil reads "facts unavailable" (recorded
	// fixtures and older outputs predate the section) — the acceptance
	// report degrades on it, never fails (docs/06-install-pipeline.md §5).
	System *SystemFacts

	// InstallerEnv reports that the session landed in a running installer
	// environment (anaconda/subiquity/d-i markers present) rather than a
	// regular system. verify_ready refuses to verify against it; discovery
	// treats it as informational.
	InstallerEnv bool `json:"installer_env"`
}

// SystemFacts are the acceptance-report facts (===SYS=== section): the
// freshly installed system's identity face — read-only, one connection,
// best-effort per line (a missing fact renders empty, never invented).
type SystemFacts struct {
	Hostname string `json:"hostname,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	// Cmdline is /proc/cmdline — the boot chain's own words (ubuntu26
	// confirmation-gate forensics read this; docs/09-roadmap.md 下一阶段 12).
	Cmdline  string `json:"cmdline,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	OS       string `json:"os,omitempty"`
	// Uptime is /proc/uptime's seconds field (raw string — precision is
	// the kernel's business).
	Uptime string `json:"uptime,omitempty"`
}

// parseSystem reads the ===SYS=== key=value lines. nil = section absent
// (facts unavailable, not empty facts).
func parseSystem(out string) *SystemFacts {
	if out == "" {
		return nil
	}
	f := &SystemFacts{}
	have := false
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), "=")
		if !ok || v == "" {
			continue
		}
		have = true
		switch k {
		case "hostname":
			f.Hostname = v
		case "kernel":
			f.Kernel = v
		case "cmdline":
			f.Cmdline = v
		case "timezone":
			f.Timezone = v
		case "os":
			f.OS = v
		case "uptime":
			f.Uptime = v
		}
	}
	if !have {
		return nil
	}
	return f
}

// Layout is the snapshot contract shape (docs/04-install-spec.md §3).
type Layout struct {
	CapturedAt time.Time    `json:"captured_at"`
	Source     string       `json:"source"`
	Disks      []LayoutDisk `json:"disks"`
	// Lvm is the machine's volume group view at capture time — the binding
	// source for storage.lvm keep:vg entries and the fresh-name collision
	// precheck. Absent in snapshots predating the section.
	Lvm []VgFacts `json:"lvm,omitempty"`
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

// VgFacts is one volume group as the installed system reports it
// (===LVM=== section): the post-install verification view AND the snapshot
// binding view for storage.lvm keep:vg (docs/04-install-spec.md §5.1).
type VgFacts struct {
	Name      string    `json:"name"`
	PVCount   int       `json:"pv_count"`
	SizeBytes int64     `json:"size_bytes"`
	PVs       []PvFacts `json:"pvs,omitempty"`
	LVs       []LvFacts `json:"lvs,omitempty"`
}

// PvFacts is one physical volume: kernel basename (sda2, nvme0n1p2, md0) —
// partition-level, like pvs reports it.
type PvFacts struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// LvFacts is one logical volume. Mount/FSType come from the lsblk tree join
// (the lvs columns do not carry them): Mount is the live mountpoint — empty
// means the LV exists but is not mounted.
type LvFacts struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	DMPath    string `json:"dm_path,omitempty"`
	Mount     string `json:"mount,omitempty"`
	FSType    string `json:"fstype,omitempty"`
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
	Name       string       `json:"name"`
	Path       string       `json:"path"`
	Size       int64        `json:"size"`
	Type       string       `json:"type"`
	FSType     string       `json:"fstype"`
	Mountpoint *string      `json:"mountpoint"`
	PartLabel  string       `json:"partlabel"`
	PartUUID   string       `json:"partuuid"`
	Children   []lsblkChild `json:"children"`
}

type blkidDev struct {
	Device string
	UUID   string
	Type   string
}

// parse converts the combined script output. Exported for tests.
func parse(out string) (*Result, error) {
	lsblkOut, blkidOut, linkOut, partsOut, bondOut, lvmOut, envOut, sysOut, err := splitSections(out)
	if err != nil {
		return nil, err
	}
	disks, err := parseLSBLK(lsblkOut)
	if err != nil {
		return nil, err
	}
	blkid := parseBlkid(blkidOut)
	starts := parseStarts(partsOut)
	lvmNodes := harvestLVMNodes(disks)

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
	lvm := parseLvm(lvmOut, lvmNodes)
	layout.Lvm = lvm
	return &Result{
		Layout:       layout,
		NICs:         nics,
		Bonds:        parseBonds(bondOut),
		Lvm:          lvm,
		System:       parseSystem(sysOut),
		InstallerEnv: strings.TrimSpace(envOut) != "",
	}, nil
}

// splitSections slices the combined output by section markers. Section
// presence is OPTIONAL after the first three (recorded fixtures and older
// collected outputs predate bond/LVM/env/sys): each marker found owns the
// bytes up to the next found marker (position-sorted, order-independent —
// a missing middle section hands its span to the previous one's follower,
// same reading as the chained form it replaced). Bond/LVM/System absence
// reads as "facts unavailable" (nil, NOT empty) — verify_ready and the
// acceptance report degrade on it; env absence reads as "not an installer".
func splitSections(out string) (lsblk, blkid, link, parts, bond, lvm, env, sys string, err error) {
	type section struct {
		tag   string
		start int
		dst   *string
	}
	secs := []section{
		{secLSBLK, strings.Index(out, secLSBLK), &blkid},
		{secBLKID, strings.Index(out, secBLKID), &link},
		{secLINK, strings.Index(out, secLINK), &parts},
		{secBOND, strings.Index(out, secBOND), &bond},
		{secLVM, strings.Index(out, secLVM), &lvm},
		{secENV, strings.Index(out, secENV), &env},
		{secSYS, strings.Index(out, secSYS), &sys},
	}
	if secs[0].start < 0 || secs[1].start < 0 || secs[2].start < 0 {
		return "", "", "", "", "", "", "", "", fmt.Errorf(
			"output missing section markers (lsblk=%v blkid=%v link=%v)",
			secs[0].start >= 0, secs[1].start >= 0, secs[2].start >= 0)
	}
	found := secs[:0]
	for _, sec := range secs {
		if sec.start >= 0 {
			found = append(found, sec)
		}
	}
	lsblk = out[:found[0].start]
	for i, sec := range found {
		end := len(out)
		if i+1 < len(found) {
			end = found[i+1].start
		}
		*sec.dst = out[sec.start+len(sec.tag) : end]
	}
	return lsblk, blkid, link, parts, bond, lvm, env, sys, nil
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

// harvestLVMNodes walks the full lsblk tree collecting type=lvm nodes (they
// nest under the partitions that hold their PVs). Key is the lsblk NAME —
// the device-mapper name (vg0-root); the ===LVM=== lvs output joins on
// vg_name-lv_name, unambiguous because mammoth's contract bans dashes in
// VG/LV names.
func harvestLVMNodes(disks []lsblkDisk) map[string]lsblkChild {
	out := map[string]lsblkChild{}
	var walk func(children []lsblkChild)
	walk = func(children []lsblkChild) {
		for _, ch := range children {
			if ch.Type == "lvm" {
				out[ch.Name] = ch
			}
			if len(ch.Children) > 0 {
				walk(ch.Children)
			}
		}
	}
	for _, d := range disks {
		walk(d.Children)
	}
	return out
}

// parseLvm reads the ===LVM=== section — three sub-sections the script only
// emits when lvm2 is present (command -v vgs gate):
//
//	---VGS---  vg0,2,21474836480B
//	---PVS---  /dev/sda2,vg0,10736369664B
//	---LVS---  vg0,root,10726316544B,/dev/dm-0
//
// Returns nil when the section is absent or lvm2 produced nothing (facts
// unavailable, NOT "no volume groups" — the verify_ready degrade path owns
// that distinction). A machine with lvm2 and no groups reads as an empty,
// non-nil slice.
func parseLvm(out string, lvmNodes map[string]lsblkChild) []VgFacts {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	var vgsBlock, pvsBlock, lvsBlock string
	rest := out
	if i := strings.Index(rest, "---VGS---"); i >= 0 {
		rest = rest[i+len("---VGS---"):]
		if j := strings.Index(rest, "---PVS---"); j >= 0 {
			vgsBlock, rest = rest[:j], rest[j+len("---PVS---"):]
			if k := strings.Index(rest, "---LVS---"); k >= 0 {
				pvsBlock, rest = rest[:k], rest[k+len("---LVS---"):]
				lvsBlock = rest
			} else {
				pvsBlock = rest
			}
		} else {
			vgsBlock = rest
		}
	} else {
		return nil // markers absent: lvm2 missing entirely
	}

	byName := map[string]*VgFacts{}
	var order []string
	for _, line := range strings.Split(vgsBlock, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		vg := &VgFacts{Name: strings.TrimSpace(f[0]), PVs: []PvFacts{}, LVs: []LvFacts{}}
		vg.PVCount, _ = strconv.Atoi(strings.TrimSpace(f[1]))
		vg.SizeBytes = parseLvmSize(f[2])
		byName[vg.Name] = vg
		order = append(order, vg.Name)
	}
	for _, line := range strings.Split(pvsBlock, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 3 || f[1] == "" {
			continue
		}
		vg, ok := byName[strings.TrimSpace(f[1])]
		if !ok {
			continue // PV orphaned from its VG row — not a verification target
		}
		pv := PvFacts{Name: path.Base(strings.TrimSpace(f[0]))}
		if len(f) > 2 {
			pv.SizeBytes = parseLvmSize(f[2])
		}
		vg.PVs = append(vg.PVs, pv)
	}
	for _, line := range strings.Split(lvsBlock, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 4 || f[0] == "" || f[1] == "" {
			continue
		}
		vg, ok := byName[strings.TrimSpace(f[0])]
		if !ok {
			continue
		}
		lv := LvFacts{Name: strings.TrimSpace(f[1]), DMPath: strings.TrimSpace(f[3])}
		lv.SizeBytes = parseLvmSize(f[2])
		if node, ok := lvmNodes[vg.Name+"-"+lv.Name]; ok {
			lv.Mount = deref(node.Mountpoint)
			lv.FSType = node.FSType
		}
		vg.LVs = append(vg.LVs, lv)
	}

	out2 := make([]VgFacts, 0, len(order))
	for _, name := range order {
		out2 = append(out2, *byName[name])
	}
	return out2
}

// parseLvmSize reads an lvm2 --units b size token ("10726316544B", possibly
// with thousands separators) into bytes; 0 when unreadable.
func parseLvmSize(tok string) int64 {
	tok = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(tok), "B"))
	tok = strings.ReplaceAll(tok, ",", "")
	n, err := strconv.ParseInt(tok, 10, 64)
	if err != nil {
		return 0
	}
	return n
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
