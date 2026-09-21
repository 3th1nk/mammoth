// Package windows implements the OSDriver for Windows Server unattended
// installation (autounattend.xml) — docs/compat/distros.md §windows.
// Mechanics: Windows Setup natively scans the boot medium root for
// autounattend.xml, so the driver renders it as a root-level answer file
// (the builder bakes seed files there and replays the media's own El Torito
// records — no bootloader config to patch, unlike the Linux dialects).
// The completion callback and declared static network ride a
// SetupComplete.cmd injected into install.wim by the builder (wimlib): it
// runs as SYSTEM before first logon with the network stack up — the Windows
// analog of the Linux dialects' late-commands (anything left to an
// interactive session would need a human).
//
// PXE carrier (wimboot over PXE): wimboot assembles the WinPE memory
// environment from the media's own boot files, and the builder augments
// boot.wim with small per-task seeds only — autounattend.xml,
// mammoth/task.json and a startnet.cmd that maps the deployment SMB export
// and launches setup from it (install.wim must NOT ride the wim: >4G
// boot.wim is rejected by the bootmgr ramdisk path, qemu-reproduced
// 2026-09-20; the SMB share itself is deployment-provided, the same shape
// as the NFS media export). Delivery is iPXE: it is the only documented
// wimboot host, so Secure Boot is out of scope for this carrier
// (docs/compat/distros.md §windows).
//
// v1 boundary (explicit, honest): UEFI-only (the rendered DiskConfiguration
// is ESP+MSR+GPT; a Legacy BIOS machine cannot consume it), no keep
// semantics, no RAID, no bond/vlan, no user scripts. SKU:
// SERVERSTANDARDCORE (Standard Core — the bare-metal default; the media
// carries every SKU, selection is by /IMAGE/NAME metadata).
package windows

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// Driver is the Windows Server unattend driver.
type Driver struct {
	distro string
}

// New returns the driver for one distro name (windows2019 today; the media
// layout is version-stable, so 2022 slots in as a constructor variant).
func New(distro string) *Driver { return &Driver{distro: distro} }

func (d *Driver) Distro() string {
	if d.distro == "" {
		return "windows2019"
	}
	return d.distro
}

// SupportedArchs is amd64-only — the media and the Windows boot chain.
func (d *Driver) SupportedArchs() []render.Arch { return []render.Arch{render.ArchAMD64} }

// Family reports the installer family for the support matrix.
func (d *Driver) Family() string { return "windows" }

// KeepPartitionSupport: unattend DiskConfiguration has no reuse semantics
// worth modeling in v1 — WillWipeDisk is the shape.
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportNone }

// PXESupport: full via the wimboot carrier — the chain (iPXE → wimboot →
// bootmgfw → WinPE) is qemu-validated and setup consumes the install source
// from the deployment SMB export mapped by the baked startnet. The share
// itself is a deployment fact: submissions gate on it being configured
// (SCHEMA_WINDOWS_SMB_SHARE_REQUIRED), not on the driver.
func (d *Driver) PXESupport() render.SupportLevel { return render.SupportFull }

// NetbootInstallDriver: the wimboot carrier, no pool — the install source
// rides the deployment SMB export the startnet maps.
func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierWimboot }
func (d *Driver) NetbootPool() render.NetbootPool       { return render.NetbootPoolNone }

// FirmwareSupport: the rendered DiskConfiguration is UEFI-shaped
// (ESP + MSR + GPT) — a Legacy BIOS machine would fail WillShowUI=OnError
// with no one watching, so the mismatch is caught at submission instead.
// The 2019 media itself boots both firmwares; this declares what THIS
// driver's unattend supports.
func (d *Driver) FirmwareSupport() render.FirmwareSupport { return render.FirmwareUEFIOnly }

// mediaLocale is the language the media carries, rendered into both the
// windowsPE and oobeSystem international components: setup and the
// installed system speak the media's language. Values must name a language
// the media actually has — the zh-CN single-language media ships no en-US
// setup resources, so en-US settings collapse into the language-selection
// page via resource-load failure even when the component itself parses.
type mediaLocale struct {
	uiLang      string
	inputLocale string
}

// mediaLocaleTable maps sources/lang.ini language tokens (the media's own
// declaration, e.g. "zh-cn") to the unattend language set: the UILanguage
// display form plus the GeoID:KLID keyboard pair. One row per language a
// deployment may plausibly meet — extend as media variants register.
var mediaLocaleTable = map[string]mediaLocale{
	"zh-cn": {uiLang: "zh-CN", inputLocale: "0804:00000804"},
	"en-us": {uiLang: "en-US", inputLocale: "0409:00000409"},
}

// defaultMediaLocale is the fallback when detection is unavailable or the
// token is unknown — the language of the media this driver was first
// registered against. Falling back here keeps behavior identical to the
// pre-detection era instead of guessing a value the media may not carry.
var defaultMediaLocale = mediaLocale{uiLang: "zh-CN", inputLocale: "0804:00000804"}

// mediaLocaleFor resolves the unattend language set for a detected media
// language token; unknown/empty inputs take defaultMediaLocale.
func mediaLocaleFor(lang string) mediaLocale {
	if ml, ok := mediaLocaleTable[strings.ToLower(strings.TrimSpace(lang))]; ok {
		return ml
	}
	return defaultMediaLocale
}

// osImageName is the /IMAGE/NAME inside install.wim the unattend selects —
// Standard Core, the bare-metal default (docs/compat/distros.md §windows:
// one media carries Standard/Datacenter × Core/Desktop; edition choice is
// metadata, not a different ISO).
func (d *Driver) osImageName() string {
	year := strings.TrimPrefix(d.Distro(), "windows")
	return "Windows Server " + year + " SERVERSTANDARDCORE"
}

// SetupCompleteSeedName is the ISO-root path the builder bakes the
// completion/network script under; the builder injects it into every
// install.wim image (wimlib) so it lands in %WINDIR%\Setup\Scripts\.
const (
	SetupCompleteSeedName = "mammoth/SetupComplete.cmd"
	CompletePS1SeedName   = "mammoth/mammoth-complete.ps1"
	TaskJSONSeedName      = "mammoth/task.json"
	// StartnetSeedName lands at \Windows\System32\startnet.cmd inside the
	// augmented boot.wim (the builder delete-then-adds it over the stock
	// wpeinit-only script): WinPE runs it at startup, it maps the install
	// share and hands off to setup.
	StartnetSeedName = "mammoth/startnet.cmd"
	// WinpeshlIniSeedName lands at \Windows\System32\winpeshl.ini and is
	// load-bearing: the Setup image's stock flow launches setup.exe itself
	// (bypassing startnet entirely — observed: our startnet never ran while
	// setup failed on the unattended DiskConfiguration), so the boot order
	// must be pinned to startnet through winpeshl's [LaunchApps].
	WinpeshlIniSeedName = "mammoth/winpeshl.ini"
)

// RenderAnswers produces autounattend.xml + the SetupComplete.cmd source.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: answer/completion URLs are required", d.distro)
	}
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: image source is required", d.distro)
	}
	if in.Netboot != nil && in.Netboot.PoolURL != "" {
		// The wimboot carrier needs no pool (the install source rides the
		// deployment SMB export); a pool URL would mean the submission was
		// shaped by another distro's logic.
		return nil, render.BootParams{}, fmt.Errorf("%s: PXE uses the wimboot carrier — no pool tree applies", d.distro)
	}
	var startnet string
	if in.Netboot != nil {
		if in.Netboot.InstallSMBUNC == "" {
			return nil, render.BootParams{}, fmt.Errorf("%s: PXE needs the deployment SMB export (MAMMOTH_WINDOWS_INSTALL_SMB_UNC)", d.distro)
		}
		var err error
		if startnet, err = startnetCmd(*in.Netboot, in.AnswerBaseURL); err != nil {
			return nil, render.BootParams{}, err
		}
	}
	if len(in.Raid) > 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: RAID is not supported yet — submit plain disks", d.distro)
	}
	if len(in.Scripts) > 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: user scripts are not supported yet (SetupComplete is engine-owned)", d.distro)
	}

	render.NormalizeESP(in.Disks)
	hostname, err := computerName(in.Hostname)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	plan, err := planDisks(d.distro, in)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	if err := validateNetwork(in.Network); err != nil {
		return nil, render.BootParams{}, err
	}

	unattend := unattendXML(d.osImageName(), hostname, in.RootPassword, mediaLocaleFor(in.MediaLanguage), plan)
	task, terr := taskJSON(in.CompleteURL, in.Network)
	if terr != nil {
		return nil, render.BootParams{}, terr
	}

	// Windows Setup finds autounattend.xml at the boot medium root by
	// itself (no ds=/inst.ks-style argument — BootParams.KernelArgs stays
	// empty). Setup reboots on its own after applying the image.
	// Per-task variance lives ONLY in task.json (ISO root): the SetupComplete
	// pair is generic, so the builder's prepared install.wim is ISO-sha
	// cacheable — task.json is consumed at first boot from the still-mounted
	// medium (the medium is released only after the completion callback).
	answers := []render.AnswerFile{
		{Name: "autounattend.xml", Content: unattend},
		{Name: SetupCompleteSeedName, Content: setupCompleteCmd()},
		{Name: CompletePS1SeedName, Content: mammothCompletePS()},
		{Name: TaskJSONSeedName, Content: task},
	}
	if startnet != "" {
		answers = append(answers, render.AnswerFile{Name: StartnetSeedName, Content: startnet})
		answers = append(answers, render.AnswerFile{Name: WinpeshlIniSeedName, Content: winpeshlIni()})
	}
	return answers, render.BootParams{
		AnswerURL:           strings.TrimSuffix(in.AnswerBaseURL, "/") + "/autounattend.xml",
		InstallerAutoReboot: true,
	}, nil
}

// diskPlan is the resolved UEFI partition layout: the declared partitions
// plus the MSR Windows requires after the ESP (mammoth-owned, 16MiB, not
// part of the spec). The OS partition (mount "/") is the InstallTo target.
type diskPlan struct {
	partitions []planPartition
	osIndex    int // position in partitions (0-based)
}

type planPartition struct {
	// kind: EFI | MSR | Primary
	kind   string
	sizeMB int
	extend bool
	format string // FAT32 | NTFS | "" (MSR: unformatted)
	label  string
	letter string // drive letter for the OS partition
	isOS   bool
}

const (
	espSizeMB = 300 // default ESP when the spec declares none (≥100 hard floor)
	msrSizeMB = 16  // Windows MSR — required on GPT, no format, no label
)

// diskPlanOf renders the spec partitions into the Windows disk shape. v1
// takes a single OS disk (the boot drive); the ESP is used when declared
// (NormalizeESP guarantees the flag) and synthesized otherwise.
func planDisks(distro string, in render.InstallInputs) (diskPlan, error) {
	var boot *render.ResolvedDisk
	for i := range in.Disks {
		if in.Disks[i].KeepDisk {
			return diskPlan{}, fmt.Errorf("%s: keep: disk is not supported on windows (SupportNone)", distro)
		}
		if !in.Disks[i].Wipe {
			return diskPlan{}, fmt.Errorf("%s: disk %s must declare wipe (keep is unsupported)", distro, in.Disks[i].Device)
		}
		if len(in.Disks[i].Baseline) > 0 || len(in.Disks[i].Remove) > 0 {
			return diskPlan{}, fmt.Errorf("%s: keep: partitions is not supported on windows (SupportNone)", distro)
		}
		for _, p := range in.Disks[i].Partitions {
			if p.Mount == "/" {
				if boot != nil {
					return diskPlan{}, fmt.Errorf("%s: more than one disk carries the OS partition", distro)
				}
				boot = &in.Disks[i]
			}
		}
	}
	if boot == nil {
		return diskPlan{}, fmt.Errorf("%s: spec declares no OS partition (mount \"/\")", distro)
	}

	plan := diskPlan{}
	espDone := false
	for i, p := range boot.Partitions {
		isESP := hasESP(p)
		isOS := p.Mount == "/"
		if isOS && plan.osIndex != 0 {
			return diskPlan{}, fmt.Errorf("%s: more than one partition mounts /", distro)
		}
		if !isESP && !isOS && p.Mount != "" && p.Mount != "swap" {
			// Non-OS mounts are drive-letter territory — Windows assigns
			// letters itself; a declared mountpoint is a spec smell in v1.
			return diskPlan{}, fmt.Errorf("%s: partition mount %q is not supported (declare the OS as /, leave others mountless)", distro, p.Mount)
		}
		if p.Preserve {
			return diskPlan{}, fmt.Errorf("%s: preserve is not supported on windows (SupportNone)", distro)
		}
		growOK := i == len(boot.Partitions)-1 // Extend leaves no room after
		if p.Grow && !growOK {
			return diskPlan{}, fmt.Errorf("%s: grow is only allowed on the last partition", distro)
		}
		if !p.Grow && p.SizeMB == 0 {
			return diskPlan{}, fmt.Errorf("%s: partition %d needs size_mb or grow", distro, i+1)
		}

		if isESP && !espDone {
			size := p.SizeMB
			if size < 100 {
				size = espSizeMB
			}
			plan.append(planPartition{kind: "EFI", sizeMB: size, format: "FAT32", label: "System"})
			plan.append(planPartition{kind: "MSR", sizeMB: msrSizeMB})
			espDone = true
			if isOS {
				return diskPlan{}, fmt.Errorf("%s: one partition cannot be both ESP and OS", distro)
			}
			continue
		}

		fs := "NTFS"
		switch strings.ToLower(p.FS) {
		case "", "ntfs":
			fs = "NTFS"
		case "vfat", "fat32", "fat":
			fs = "FAT32"
		default:
			return diskPlan{}, fmt.Errorf("%s: filesystem %q is not a windows filesystem (ntfs|fat32)", distro, p.FS)
		}
		part := planPartition{kind: "Primary", sizeMB: p.SizeMB, extend: p.Grow, format: fs, isOS: isOS}
		if isOS {
			part.label = "Windows"
			part.letter = "C"
		} else if isESP {
			return diskPlan{}, fmt.Errorf("%s: a second ESP is not supported", distro)
		} else {
			part.label = "Data" + fmt.Sprint(i+1)
		}
		plan.append(part)
		if isOS {
			plan.osIndex = len(plan.partitions) - 1
		}
	}
	if !espDone {
		// Defensive: NormalizeESP covers /boot/efi mounts, a bare layout
		// without any ESP still boots via the ESP-fallback — Windows needs
		// the real thing, so synthesize ESP+MSR ahead of everything.
		prepended := []planPartition{
			{kind: "EFI", sizeMB: espSizeMB, format: "FAT32", label: "System"},
			{kind: "MSR", sizeMB: msrSizeMB},
		}
		plan.partitions = append(prepended, plan.partitions...)
		plan.osIndex += len(prepended)
	}
	return plan, nil
}

func (p *diskPlan) append(part planPartition) { p.partitions = append(p.partitions, part) }

func hasESP(p render.ResolvedPartition) bool {
	if p.Mount == "/boot/efi" {
		return true
	}
	for _, f := range p.Flags {
		if f == "esp" {
			return true
		}
	}
	return false
}

// computerName sanitizes the hostname into NetBIOS shape: [A-Za-z0-9-],
// ≤15 chars (the label DNS label limit Windows enforces on ComputerName).
func computerName(hostname string) (string, error) {
	var b strings.Builder
	for _, r := range hostname {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '.' || r == '_':
			b.WriteRune('-')
		default:
			return "", fmt.Errorf("hostname %q: character %q is not valid in a Windows computer name", hostname, r)
		}
	}
	name := b.String()
	if name == "" {
		return "", fmt.Errorf("hostname %q renders an empty computer name", hostname)
	}
	if len(name) > 15 {
		name = name[:15]
	}
	return name, nil
}

// validateNetwork rejects the shapes SetupComplete cannot express in v1:
// bonds/vlans (no server-side teardown here), extra static routes, and
// entries without a MAC (the NIC binding is by MAC — interface names vary
// across locales and hardware).
func validateNetwork(entries []render.NetworkEntry) error {
	for i, e := range entries {
		if e.Bond != nil || e.VLAN != nil {
			return fmt.Errorf("windows: network entry %d: bond/vlan is not supported yet", i+1)
		}
		if e.Match == nil || e.Match.MAC == "" {
			return fmt.Errorf("windows: network entry %d must match a MAC (the SetupComplete binding is by MAC)", i+1)
		}
		for _, r := range e.Routes {
			if r.To != "default" && r.To != "0.0.0.0/0" {
				return fmt.Errorf("windows: network entry %d: non-default route %q is not supported yet", i+1, r.To)
			}
		}
	}
	return nil
}

// setupCompleteCmd is the GENERIC launcher injected into install.wim —
// per-task content must never bake into it, or the sha-cached prepared wim
// breaks (task variance arrives via mammoth/task.json on the medium).
func setupCompleteCmd() string {
	return "@echo off\n" +
		"rem mammoth: config arrives via mammoth\\task.json on the install medium\n" +
		"powershell -NoProfile -ExecutionPolicy Bypass -File \"%~dp0mammoth-complete.ps1\"\n"
}

// mammothCompletePS is the generic first-boot logic (SYSTEM, pre-logon):
// read the per-task config from the still-mounted medium, land the declared
// static network (the install ran from local media — the target-side write
// IS the network config, ubuntu late-command netplan precedent), then fire
// the completion callback — which is what releases that very medium.
func mammothCompletePS() string {
	return `# mammoth first-boot: completion callback + declared network.
$ErrorActionPreference = "SilentlyContinue"
$cfg = $null
if (Test-Path (Join-Path $PSScriptRoot "task.json")) {
    $cfg = Get-Content (Join-Path $PSScriptRoot "task.json") -Raw | ConvertFrom-Json
} else {
    foreach ($d in (Get-PSDrive -PSProvider FileSystem).Root) {
        $p = Join-Path $d "mammoth\task.json"
        if (Test-Path $p) { $cfg = Get-Content $p -Raw | ConvertFrom-Json; break }
    }
}
if ($cfg) {
    foreach ($n in $cfg.network) {
        $nic = Get-NetAdapter | Where-Object { $_.MacAddress -eq $n.mac }
        if ($nic) {
            $first = $true
            foreach ($a in $n.ips) {
                if ($first -and $n.gateway) {
                    New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress $a.ip -PrefixLength ([int]$a.prefix) -DefaultGateway $n.gateway | Out-Null
                } else {
                    New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress $a.ip -PrefixLength ([int]$a.prefix) | Out-Null
                }
                $first = $false
            }
            if ($n.dns) { Set-DnsClientServerAddress -InterfaceIndex $nic.ifIndex -ServerAddresses $n.dns }
        }
    }
    try {
        Invoke-WebRequest -Uri $cfg.complete_url -Method POST -Body '{"status":"ok","detail":"windows setup finished"}' -ContentType 'application/json' -UseBasicParsing -TimeoutSec 15 | Out-Null
    } catch {}
}
`
}

// taskConfig is the per-task runtime contract consumed at first boot.
type taskConfig struct {
	CompleteURL string       `json:"complete_url"`
	Network     []taskNetNIC `json:"network,omitempty"`
}

type taskNetNIC struct {
	MAC     string   `json:"mac"`
	IPs     []taskIP `json:"ips"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

type taskIP struct {
	IP     string `json:"ip"`
	Prefix string `json:"prefix"`
}

// taskJSON renders the per-task config (ISO root — the only per-task seed).
func taskJSON(completeURL string, entries []render.NetworkEntry) (string, error) {
	cfg := taskConfig{CompleteURL: completeURL}
	for _, e := range entries {
		if len(e.Addresses) == 0 {
			continue
		}
		nic := taskNetNIC{MAC: normalizeMAC(e.Match.MAC)}
		for i, a := range e.Addresses {
			p := strings.SplitN(a, "/", 2)
			prefix := "32"
			if len(p) == 2 && p[1] != "" {
				prefix = p[1]
			}
			nic.IPs = append(nic.IPs, taskIP{IP: p[0], Prefix: prefix})
			if i == 0 {
				for _, r := range e.Routes {
					if r.To == "default" || r.To == "0.0.0.0/0" {
						nic.Gateway = r.Via
					}
				}
			}
		}
		nic.DNS = e.Nameservers
		cfg.Network = append(cfg.Network, nic)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("windows: task.json: %w", err)
	}
	return string(b) + "\n", nil
}

// normalizeMAC maps a spec MAC (any separator) into Get-NetAdapter's
// display form (uppercase, hyphen-separated).
func normalizeMAC(mac string) string {
	clean := strings.Map(func(r rune) rune {
		if r == ':' || r == '.' || r == '-' {
			return -1
		}
		return r
	}, strings.ToUpper(mac))
	var b strings.Builder
	for i, r := range clean {
		if i > 0 && i%2 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unattendXML renders autounattend.xml. Built as explicit XML (not
// encoding/xml structs): the schema is attribute-annotated magic
// (wcm:action, component names) that reads better verbatim; every dynamic
// value passes xmlEscape.
//
// The windowsPE component is Microsoft-Windows-International-Core-WinPE —
// NOT "Microsoft-Windows-International-WinPE". The wrong name is invisible
// at the XML layer: setup parses the file fine, but SMI rejects the whole
// component ("no component matches the given namespace" in the setupact
// SMI dump), the setup/target language stay undetermined and setup shows
// the language-selection page no matter what values the component carried
// (real-media setupact, 9/21: "Could not determine Target language. Will
// ask to show UI").
func unattendXML(imageName, hostname, password string, ml mediaLocale, plan diskPlan) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s) }
	w(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <SetupUILanguage>
        <UILanguage>` + ml.uiLang + `</UILanguage>
      </SetupUILanguage>
      <InputLocale>` + ml.inputLocale + `</InputLocale>
      <SystemLocale>` + ml.uiLang + `</SystemLocale>
      <UILanguage>` + ml.uiLang + `</UILanguage>
      <UserLocale>` + ml.uiLang + `</UserLocale>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <DiskConfiguration>
        <WillShowUI>Never</WillShowUI>
        <Disk wcm:action="add">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
`)
	for i, p := range plan.partitions {
		order := i + 1
		switch p.kind {
		case "MSR":
			w(fmt.Sprintf("            <CreatePartition wcm:action=\"add\">\n              <Order>%d</Order>\n              <Type>MSR</Type>\n              <Size>%d</Size>\n            </CreatePartition>\n", order, p.sizeMB))
		default:
			sizeTag := fmt.Sprintf("<Size>%d</Size>", p.sizeMB)
			if p.extend {
				sizeTag = "<Extend>true</Extend>"
			}
			w(fmt.Sprintf("            <CreatePartition wcm:action=\"add\">\n              <Order>%d</Order>\n              <Type>%s</Type>\n              %s\n            </CreatePartition>\n", order, p.kind, sizeTag))
		}
	}
	w(`          </CreatePartitions>
          <ModifyPartitions>
`)
	// ModifyPartition's Order counts ModifyPartitions entries (1..N, MSR has
	// none), while PartitionID is the on-disk partition number (MSR occupies
	// one) — conflating the two skips an Order and setup rejects the whole
	// DiskConfiguration with 0x8007000d (real-media: qemu SMB round, 9/21).
	modOrder := 0
	for i, p := range plan.partitions {
		if p.kind == "MSR" {
			continue // unformatted by definition
		}
		modOrder++
		order, pid := modOrder, i+1
		letter := ""
		if p.letter != "" {
			letter = fmt.Sprintf("\n              <Letter>%s</Letter>", p.letter)
		}
		w(fmt.Sprintf("            <ModifyPartition wcm:action=\"add\">\n              <Order>%d</Order>\n              <PartitionID>%d</PartitionID>\n              <Format>%s</Format>\n              <Label>%s</Label>%s\n            </ModifyPartition>\n",
			order, pid, p.format, xmlEscape(p.label), letter))
	}
	w(`          </ModifyPartitions>
        </Disk>
      </DiskConfiguration>
      <ImageInstall>
        <OSImage>
          <InstallFrom>
            <MetaData wcm:action="add">
              <Key>/IMAGE/NAME</Key>
              <Value>` + xmlEscape(imageName) + `</Value>
            </MetaData>
          </InstallFrom>
          <InstallTo>
            <DiskID>0</DiskID>
            <PartitionID>` + fmt.Sprint(plan.osIndex+1) + `</PartitionID>
          </InstallTo>
        </OSImage>
      </ImageInstall>
      <!-- ProductKey: the element must exist (setup aborts reading the
           unattend without it) but the Key stays empty BY DESIGN — with no
           key setup resolves the channel from sources\ei.cfg, which the
           builder drops into the prepared tree ([Channel] Volume). A key
           here (even Microsoft's public KMS client setup key) sends setup
           down the key-validation path instead, which fails in the network
           launch shape (0xC004F050, qemu 9/21) and pops the product-key
           page with no one to click it. -->
      <UserData>
        <AcceptEula>true</AcceptEula>
        <ProductKey>
          <Key></Key>
        </ProductKey>
      </UserData>
    </component>
  </settings>
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <ComputerName>` + xmlEscape(hostname) + `</ComputerName>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <InputLocale>` + ml.inputLocale + `</InputLocale>
      <SystemLocale>` + ml.uiLang + `</SystemLocale>
      <UILanguage>` + ml.uiLang + `</UILanguage>
      <UserLocale>` + ml.uiLang + `</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
      </OOBE>
      <!-- Primary completion trigger: RunSynchronous is a native unattend
           mechanism and oobeSystem is proven to run on this path
           (AdministratorPassword takes effect). SetupComplete remains as
           backup — its auto-execution silently never fired on the real
           machine (setupact has zero records, root cause open). The ps1 is
           idempotent: it re-configures the declared network and re-POSTs
           the callback, so running from both paths is harmless. -->
      <RunSynchronous>
        <RunSynchronousCommand wcm:action="add">
          <Order>1</Order>
          <Path>powershell -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Setup\Scripts\mammoth-complete.ps1</Path>
          <Description>mammoth static network + completion callback</Description>
        </RunSynchronousCommand>
      </RunSynchronous>
      <UserAccounts>
        <AdministratorPassword>
          <Value>` + xmlEscape(password) + `</Value>
          <PlainText>true</PlainText>
        </AdministratorPassword>
      </UserAccounts>
    </component>
  </settings>
</unattend>
`)
	return b.String()
}

// startnetCmd renders the WinPE startup script baked into boot.wim
// (\Windows\System32\startnet.cmd, delete-then-added over the stock
// wpeinit-only script): bring the network up, map the deployment SMB
// export, and launch setup against the answer file already on the ramdisk
// (X:). Setup resolves install.wim from its own launch location
// (<share>\sources), so the WDS-style flow needs no per-image arguments.
// Every dynamic value is character-validated — cmd has no safe quoting for
// metacharacters, so the config contract is a restricted charset instead.
func startnetCmd(in render.NetbootInputs, answerBase string) (string, error) {
	if err := validateShareToken(in.InstallSMBUNC, "share UNC", true); err != nil {
		return "", err
	}
	if err := validateShareToken(in.InstallSMBUser, "share user", false); err != nil {
		return "", err
	}
	if err := validateShareToken(in.InstallSMBPassword, "share password", false); err != nil {
		return "", err
	}
	// The image path is repo-relative and machine-generated (pool-store sha
	// addressing) — validated anyway, it flows into a batch script.
	if err := validateInstallImagePath(in.InstallSMBImagePath); err != nil {
		return "", err
	}
	cred := ""
	if in.InstallSMBUser != "" {
		cred = fmt.Sprintf(" \"%s\" /user:%s", in.InstallSMBPassword, in.InstallSMBUser)
	} else {
		// An empty-credential net use sends a NULL session, which the client
		// side refuses outright (startnet would silently never map the
		// share) — guest + empty password rides the server's guest mapping.
		cred = " \"\" /user:guest"
	}
	var b strings.Builder
	w := func(line string) { b.WriteString(line); b.WriteByte('\n') }
	w("@echo off")
	w("rem mammoth: wimboot carrier - map the deployment install share and launch setup.")
	if in.InstallSMBUser == "" {
		// The WinPE SMB client refuses sessions the server maps to guest
		// (AllowInsecureGuestAuth defaults to off and WinPE has no Group
		// Policy) — the docs-sanctioned opt-in is a direct registry write.
		// It MUST run before wpeinit: the SMB redirector reads the key when
		// the workstation stack starts (real-hardware round, 9/21).
		w(`reg add "HKLM\SYSTEM\CurrentControlSet\Services\LanmanWorkstation\Parameters" /v AllowInsecureGuestAuth /t REG_DWORD /d 1 /f >nul`)
	}
	w("wpeinit")
	w("set ATTEMPT=0")
	w(":waitnet")
	w("set /a ATTEMPT+=1")
	// stderr stays visible: the failure round prints the net use error on
	// screen before the drop-to-shell, which is the only diagnostic surface
	// a headless rig has.
	w(fmt.Sprintf("net use Z: %s%s >nul", in.InstallSMBUNC, cred))
	w("if not errorlevel 1 goto mounted")
	w("if %ATTEMPT% GEQ 45 (")
	w("  echo mammoth: could not map the install share - dropping to shell for diagnosis")
	w("  exit /b 1")
	w(")")
	w("ping -n 3 127.0.0.1 >nul")
	w("goto waitnet")
	w(":mounted")
	// /unattend explicit: the ramdisk root is not a setup search root, and the
	// implicit autounattend discovery does not run for a network launch.
	// Setup runs in its own window; this script then streams setup's logs
	// back to the engine. A successful install reboots the machine and ends
	// the stream naturally; a failed one leaves the Panther logs on the
	// engine for post-mortem (the DiskConfiguration investigation channel).
	// No /unattend argument: the answer file sits at the ramdisk root (X:)
	// where setup's implicit autounattend discovery finds it — the explicit
	// form changes language handling (a media-language mismatch makes setup
	// show the language picker even with the setting specified).
	w(fmt.Sprintf("start \"mammoth setup\" /D Z:\\%s Z:\\%s\\sources\\setup.exe",
		in.InstallSMBImagePath, in.InstallSMBImagePath))
	// One-shot diagnostics dump right after launch: the Panther logs may
	// live somewhere the diag loop does not poll, so the directory listings
	// themselves travel back (via the writable share) and point at the real
	// location. setuperr.log, when it appears, is picked up by the loop.
	w("if not exist Z:\\diag\\ mkdir Z:\\diag")
	for _, d := range []struct{ src, name string }{
		{`X:\Windows\Panther`, `panther-dir.txt`},
		{`X:\Windows`, `x-windows-dir.txt`},
		{`X:\`, `x-root-dir.txt`},
		{`Z:\sources`, `z-sources-dir.txt`},
	} {
		w(fmt.Sprintf("dir /b %s > Z:\\diag\\%s 2>&1", d.src, d.name))
	}
	diag := strings.TrimSuffix(answerBase, "/") + "/diag"
	w("set DIAG=0")
	w(":diag")
	w("set /a DIAG+=1")
	for _, p := range []string{
		`X:\Windows\Panther\setuperr.log`,
		`X:\Windows\Panther\setupact.log`,
		`X:\Windows\inf\setupapi.dev.log`,
	} {
		name := p[strings.LastIndex(p, "\\")+1:]
		// The upload endpoint is POST-only (machine plane, token-as-
		// credential): -X POST --data-binary, not -T (PUT 405s silently
		// under -sf, and the leg never fired).
		w(fmt.Sprintf("if exist %s curl -sf -X POST --data-binary @%s %s/%s >nul 2>&1", p, p, diag, name))
		// curl.exe is not guaranteed present in every WinPE build — the SMB
		// copy is the second leg (fires when the export allows writes).
		w(fmt.Sprintf("if exist %s copy /Y %s Z:\\diag\\%s >nul 2>&1", p, p, name))
	}
	// Ship the per-task config onto the target volume the moment setup has
	// applied the image (its auto-reboot then runs SetupComplete with
	// task.json sitting next to mammoth-complete.ps1). The wimboot ramdisk
	// (X:) dies at that same reboot — without this copy the first-boot
	// script finds no config and neither the static network nor the
	// completion callback ever fire (2288H round, 9/21). Polling every diag
	// round makes the copy idempotent; before apply the drive letters do
	// not exist and every line no-ops.
	w(`for %%d in (C D E F) do if exist %%d:\Windows\Setup\Scripts\mammoth-complete.ps1 copy /Y X:\mammoth\task.json %%d:\Windows\Setup\Scripts\task.json >nul 2>&1`)
	w("if %DIAG% GEQ 600 exit /b 0")
	w("ping -n 4 127.0.0.1 >nul")
	w("goto diag")
	return b.String(), nil
}

// validateShareToken keeps cmd metacharacters out of the rendered startnet:
// the script is batch-interpreted as SYSTEM, so the share identity is
// restricted to a safe charset rather than quoted.
func validateShareToken(v, what string, isUNC bool) error {
	if v == "" {
		if isUNC {
			return fmt.Errorf("windows: install share UNC is empty")
		}
		return nil // user/password absent: guest export
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '@':
		case isUNC && r == '\\':
		default:
			return fmt.Errorf("windows: install share %s contains character %q — cmd cannot quote metacharacters, use [A-Za-z0-9._-] (%s: also \\ and @)", what, r, what)
		}
	}
	if isUNC && !strings.HasPrefix(v, `\\`) {
		return fmt.Errorf("windows: install share UNC must start with \\\\ (got %q)", v)
	}
	return nil
}

// winpeshlIni pins the WinPE startup to our startnet: without it the Setup
// image launches setup.exe on its own and startnet never runs (the stock
// image carries no winpeshl.ini — winpeshl.exe falls back to startnet only
// after nothing else is launched, and the Setup image IS something else).
func winpeshlIni() string {
	return "[LaunchApps]\r\n%SYSTEMDRIVE%\\Windows\\System32\\startnet.cmd\r\n"
}

// validateInstallImagePath checks the share-relative tree path: batch-safe
// charset plus the backslash separator, no drive, no leading slash, and no
// .. segments (it lands verbatim in a batch script).
func validateInstallImagePath(p string) error {
	if p == "" {
		return fmt.Errorf("windows: install image path is empty")
	}
	if strings.HasPrefix(p, "\\") || strings.Contains(p, "..") {
		return fmt.Errorf("windows: install image path %q must be repo-relative without .. segments", p)
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '\\':
		default:
			return fmt.Errorf("windows: install image path contains character %q", r)
		}
	}
	return nil
}

// xmlEscape keeps dynamic values out of the markup's way (attributes and
// text both).
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	)
	return r.Replace(s)
}
