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
// boot.wim with autounattend.xml + mammoth/task.json (small files only —
// install.wim must NOT ride the wim: >4G boot.wim is rejected by the
// bootmgr ramdisk path, qemu-reproduced 2026-09-20). The install source
// rides the network (SMB share, pending); until it lands PXESupport stays
// none and the delivery machinery (proxyDHCP routing the right MACs to
// plain iPXE, the wimboot-shaped script render) sits ready behind the
// gate. Delivery is iPXE: it is the only documented wimboot host, so
// Secure Boot is out of scope for this carrier (docs/compat/distros.md
// §windows).
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

// PXESupport: none — the wimboot carrier chain itself is validated (iPXE →
// wimboot → bootmgfw → WinPE, qemu 2026-09-20), but the install source is
// not landed yet: baking install.wim into the augmented boot.wim crosses the
// 4 GiB line and Server 2019's bootmgr ramdisk path rejects it outright
// (0xc0000225 winload.efi, qemu-reproduced), so the source must be served
// over the network (SMB share — the WDS shape) before submissions can
// complete. Flips to full when the SMB pool lands
// (docs/compat/distros.md §windows).
func (d *Driver) PXESupport() render.SupportLevel { return render.SupportNone }

// NetbootInstallDriver: the wimboot carrier, no pool — declared for the
// delivery machinery even while PXESupport keeps the gate shut.
func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierWimboot }
func (d *Driver) NetbootPool() render.NetbootPool       { return render.NetbootPoolNone }

// FirmwareSupport: the rendered DiskConfiguration is UEFI-shaped
// (ESP + MSR + GPT) — a Legacy BIOS machine would fail WillShowUI=OnError
// with no one watching, so the mismatch is caught at submission instead.
// The 2019 media itself boots both firmwares; this declares what THIS
// driver's unattend supports.
func (d *Driver) FirmwareSupport() render.FirmwareSupport { return render.FirmwareUEFIOnly }

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
		// The wimboot carrier needs no pool (the augmented boot.wim carries
		// the install source); a pool URL would mean the submission was
		// shaped by another distro's logic.
		return nil, render.BootParams{}, fmt.Errorf("%s: PXE uses the wimboot carrier — no pool tree applies", d.distro)
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

	unattend := unattendXML(d.osImageName(), hostname, in.RootPassword, plan)
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
	return []render.AnswerFile{
			{Name: "autounattend.xml", Content: unattend},
			{Name: SetupCompleteSeedName, Content: setupCompleteCmd()},
			{Name: CompletePS1SeedName, Content: mammothCompletePS()},
			{Name: TaskJSONSeedName, Content: task},
		}, render.BootParams{
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
foreach ($d in (Get-PSDrive -PSProvider FileSystem).Root) {
    $p = Join-Path $d "mammoth\task.json"
    if (Test-Path $p) { $cfg = Get-Content $p -Raw | ConvertFrom-Json; break }
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
func unattendXML(imageName, hostname, password string, plan diskPlan) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s) }
	w(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-WinPE" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <SetupUILanguage>
        <UILanguage>en-US</UILanguage>
      </SetupUILanguage>
      <InputLocale>0409:00000409</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <DiskConfiguration>
        <WillShowUI>OnError</WillShowUI>
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
	for i, p := range plan.partitions {
		if p.kind == "MSR" {
			continue // unformatted by definition
		}
		order, pid := i+1, i+1
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
      <!-- empty ProductKey: Server 2019 setup REQUIRES the element to be
           present even with /IMAGE/NAME edition selection — its absence
           aborts unattend with "cannot read the <ProductKey> setting"
           (real-media finding, 2026-09-20) -->
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
      <InputLocale>0409:00000409</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
      </OOBE>
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

// xmlEscape keeps dynamic values out of the markup's way (attributes and
// text both).
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	)
	return r.Replace(s)
}
