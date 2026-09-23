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
// semantics, no RAID, no bond/vlan. User scripts land v1 with runtime-seat
// bounds: pre_install = setup-pathway WinPE cmd only (agent pathway's
// pre-apply runtime is the busybox agent — Linux semantics, rejected
// there), post_install = installed OS via the engine-managed first-boot
// chain. SKU: SERVERSTANDARDCORE (Standard Core — the bare-metal default;
// the media carries every SKU, selection is by /IMAGE/NAME metadata).
package windows

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/render/agent"
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

// NetbootCarrierFor is the installer-aware carrier choice: the setup.exe
// flow (default) boots the wimboot carrier; the agent apply-image path
// (boot.installer=agent) rides the proven alpine agent carrier instead —
// the machine boots the agent, which wimlib-applies install.wim onto the
// declarative NTFS volume and pre-bakes the BCD (docs/compat/distros.md
// §windows, 通路路线决策: 终局 = agent apply-image).
func (d *Driver) NetbootCarrierFor(in render.InstallInputs) render.NetbootCarrier {
	if in.Installer == "agent" {
		return render.NetbootCarrierAlpineNetboot
	}
	return render.NetbootCarrierWimboot
}

// WindowsOSImageName reports the /IMAGE/NAME this driver installs —
// provision resolves the install.wim image index server-side for the agent
// apply-image plan (the agent gets a deterministic index, no SKU probing
// on the machine).
func (d *Driver) WindowsOSImageName() string { return d.osImageName() }

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
	// PreScriptSeedFmt is the ISO-root / boot.wim seat of pre_install
	// segments (setup pathway only): the unattend windowsPE RunSynchronous
	// locator scans the WinPE drive letters for exactly these names.
	PreScriptSeedFmt = "mammoth/pre-%d.cmd"
)

// scriptShell returns the entry's effective interpreter (contract default:
// cmd — docs/04-install-spec.md §5).
func scriptShell(s render.ScriptEntry) string {
	if s.Shell == "" {
		return "cmd"
	}
	return s.Shell
}

// splitScripts validates user scripts against the windows runtime seats and
// splits them by stage. pre_install runs in WinPE (setup pathway only:
// cmd-only, inline-only — WinPE ships no PowerShell and vMedia's boot.wim no
// fetcher), post_install runs on the installed OS via the engine-managed
// first-boot chain (cmd or powershell, inline or url). The agent apply path
// has NO WinPE seat — its pre-apply runtime is the busybox agent (Linux
// semantics) — so its caller rejects pre entries outright.
func splitScripts(distro string, scripts []render.ScriptEntry) (pre, post []render.ScriptEntry, err error) {
	for i, s := range scripts {
		switch s.Stage {
		case "pre_install":
			if shell := scriptShell(s); shell != "cmd" {
				return nil, nil, fmt.Errorf("%s: scripts[%d] pre_install shell=%q is not supported — WinPE ships no PowerShell, use shell=cmd", distro, i, shell)
			}
			if s.URL != "" {
				return nil, nil, fmt.Errorf("%s: scripts[%d] pre_install url form is not supported — WinPE has no fetcher on every carrier, inline the body", distro, i)
			}
			pre = append(pre, s)
		case "post_install":
			if shell := scriptShell(s); shell != "cmd" && shell != "powershell" {
				return nil, nil, fmt.Errorf("%s: scripts[%d] shell %q is not one of cmd|powershell", distro, i, shell)
			}
			post = append(post, s)
		default:
			return nil, nil, fmt.Errorf("%s: unknown script stage %q", distro, s.Stage)
		}
	}
	return pre, post, nil
}

// RenderAnswers produces autounattend.xml + the SetupComplete.cmd source.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: answer/completion URLs are required", d.distro)
	}
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: image source is required", d.distro)
	}
	for _, c := range in.Capabilities {
		switch c {
		case "rdp", "winrm", "ping":
		default:
			return nil, render.BootParams{}, fmt.Errorf("%s: access.capabilities %q is not one of rdp|winrm|ping", d.distro, c)
		}
	}
	if in.Installer == "agent" {
		return d.renderAgentApply(in)
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
	preScripts, postScripts, err := splitScripts(d.distro, in.Scripts)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	if len(in.SSHPublicKeys) > 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: access.ssh_keys is not supported (Server Core has no sshd by default)", d.distro)
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

	unattend := unattendXML(d.osImageName(), hostname, in.RootPassword, mediaLocaleFor(in.MediaLanguage), plan, preSyncCommands(len(preScripts)))
	task, terr := taskJSON(in.CompleteURL, in.Network, in.Capabilities, postScripts)
	if terr != nil {
		return nil, render.BootParams{}, terr
	}

	// Windows Setup finds autounattend.xml at the boot medium root by
	// itself (no ds=/inst.ks-style argument — BootParams.KernelArgs stays
	// empty). Setup reboots on its own after applying the image.
	// Per-task variance lives in task.json (ISO root) plus the pre_install
	// answer files when present: the SetupComplete pair is generic, so the
	// builder's prepared install.wim is ISO-sha cacheable — task.json is
	// consumed at first boot from the still-mounted medium (the medium is
	// released only after the completion callback).
	answers := []render.AnswerFile{
		{Name: "autounattend.xml", Content: unattend},
		{Name: SetupCompleteSeedName, Content: setupCompleteCmd()},
		{Name: CompletePS1SeedName, Content: mammothCompletePS()},
		{Name: TaskJSONSeedName, Content: task},
	}
	for i, s := range preScripts {
		answers = append(answers, render.AnswerFile{
			Name:    fmt.Sprintf(PreScriptSeedFmt, i+1),
			Content: s.Inline,
		})
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

// ── agent apply-image pathway (boot.installer=agent) ─────────────────────────
//
// The endgame carrier from the 通路路线决策 (docs/compat/distros.md §windows),
// in its TWO-BOOT shape (方案 A — 复刻 setup.exe 时序, 2026-09-22): boot one
// is the alpine agent, which wimlib-applies the prepared install.wim onto
// the declarative NTFS volume and injects the first-boot config in-wim
// (unattend into %WINDIR%\Panther, task.json, SpBcd-stripped Specialize.xml);
// it leaves the ESP EMPTY — a hand-made BCD store is never valid to NT's
// BcdOpenStore (0xC0000098, real-machine 9/22, every patch angle excluded),
// so boot two is a wimboot WinPE whose startnet runs bcdboot to generate
// the store natively — exactly what setup.exe itself does before rebooting
// into specialize. First boot then runs specialize/oobeSystem like the
// setup.exe flow and the callback face is byte-identical — verify_ready
// cannot tell the pathways apart (and must not).
//
// What the pathway deliberately does NOT carry: driver injection (wimlib
// lays down files; boot-critical drivers must ship in the media's inbox —
// machines outside that envelope stay on the setup.exe path, which is why
// that pathway is kept, not deleted).

// renderAgentApply renders the agent-flavored answer set: an agent-plan.sh
// in the shared mammoth_* line format (the runtime's collectors parse it
// unchanged), the Panther unattend, the per-task task.json — plus the
// boot-two (bcdboot WinPE) startnet pair, which no machine component ever
// fetches by name: the orchestration bakes them into the second boot tree's
// boot.wim at the applied marker (the names are the wimboot seed contract's,
// so the answer map passes through as the seed map verbatim).
func (d *Driver) renderAgentApply(in render.InstallInputs) ([]render.AnswerFile, render.BootParams, error) {
	if in.Netboot == nil {
		return nil, render.BootParams{}, fmt.Errorf("%s: the agent apply path is PXE-only (boot.strategy=pxe)", d.distro)
	}
	if in.Netboot.InstallWimURL == "" || in.Netboot.InstallWimIndex <= 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: agent apply needs the prepared install.wim URL and image index", d.distro)
	}
	if in.Netboot.InstallSMBUNC != "" || in.Netboot.PoolURL != "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: agent apply consumes the HTTP win tree — SMB/pool inputs are the setup path's shape", d.distro)
	}
	if len(in.Raid) > 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: RAID is not supported yet — submit plain disks", d.distro)
	}
	preScripts, postScripts, err := splitScripts(d.distro, in.Scripts)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	if len(preScripts) > 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: pre_install scripts are not supported on the agent apply path — its pre-apply runtime is the busybox agent (Linux semantics), not WinPE; use boot.installer=setup for pre_install", d.distro)
	}
	if len(in.SSHPublicKeys) > 0 {
		return nil, render.BootParams{}, fmt.Errorf("%s: access.ssh_keys is not supported (Server Core has no sshd by default)", d.distro)
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
	bootDev := bootDiskDevice(d.distro, in)

	task, terr := taskJSON(in.CompleteURL, in.Network, in.Capabilities, postScripts)
	if terr != nil {
		return nil, render.BootParams{}, terr
	}
	ml := mediaLocaleFor(in.MediaLanguage)
	planSH := winAgentPlanSH(in, bootDev, plan)
	// task.json rides a FLAT answer name here: the agent fetches it via
	// /render/<token>/<file>, a single path segment — the setup path's
	// "mammoth/task.json" (a baked seed, never fetched) does not fit the
	// route and 401s (real-machine finding, 9/22).
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")
	answers := []render.AnswerFile{
		{Name: "agent-plan.sh", Content: planSH},
		{Name: "agent-plan.json", Content: winAgentPlanJSON(d, in, bootDev, plan)},
		{Name: "unattend-panther.xml", Content: pantherUnattendXML(hostname, in.RootPassword, ml)},
		{Name: AgentTaskJSONName, Content: task},
		{Name: SpecializeXMLSeedName, Content: in.SpecializeXML},
		// Boot two (bcdboot WinPE): the seed pair under the wimboot seed
		// contract's names — the orchestration passes the whole answer map
		// as BuildWindowsWimboot's seed and only these two apply.
		{Name: StartnetSeedName, Content: bcdBootStartnetCmd(base + "/diag")},
		{Name: WinpeshlIniSeedName, Content: winpeshlIni()},
	}
	return answers, render.BootParams{
		AnswerURL:           base + "/agent-plan.sh",
		InstallerAutoReboot: true,
		// The alpine carrier's boot shape — identical to the Linux agent
		// pilot's (modloop / apks / overlay / plan base), rendered by the
		// agent package so both dialects share one argument authority.
		NetbootKernelArgs: agent.NetbootKernelArgs(in),
	}, nil
}

// AgentTaskJSONName is the flat answer-file name for the per-task task.json
// on the agent apply path (single path segment — see renderAgentApply).
const AgentTaskJSONName = "task.json"

// SpecializeXMLSeedName is the flat answer-file name for the SpBcd-stripped
// sysprep Specialize.xml the agent injects into the applied wim.
const SpecializeXMLSeedName = "win-specialize.xml"

// BCDBootDiagMarker is the diag file name the boot-two (bcdboot WinPE)
// startnet POSTs its verdict under; the orchestration polls
// <MediaDir>/diag/<taskID>/<name> for it.
const BCDBootDiagMarker = "bcdboot-done.txt"

// The marker's verdict prefixes — the orchestration's grep contract. OK
// requires BOTH bcdboot and the bcdedit /enum all gate (the acceptance
// experiment: a store NT's BCD layer cannot open is no store) to exit 0.
const (
	BCDBootMarkerOK   = "BCDBOOT OK"
	BCDBootMarkerFail = "BCDBOOT FAIL"
)

// bootDiskDevice returns the resolved device carrying the OS partition —
// the one disk the apply flow wipes and repartitions (v1: single OS disk,
// the same constraint planDisks enforces).
func bootDiskDevice(distro string, in render.InstallInputs) string {
	for i := range in.Disks {
		for _, p := range in.Disks[i].Partitions {
			if p.Mount == "/" {
				return in.Disks[i].Device
			}
		}
	}
	return "" // unreachable: planDisks already rejected the no-OS spec
}

// winAgentPlanSH renders the windows flavor of the agent plan: the shared
// mammoth_disk/mammoth_partition/mammoth_network collector lines (the
// runtime parses them unchanged) plus the MAMMOTH_WIN_* facts the apply
// branch consumes. Partition lines describe the FINAL on-disk order —
// ESP, the synthesized MSR (fs "-": unformatted), the NTFS OS volume —
// mirroring the DiskConfiguration the setup path renders.
func winAgentPlanSH(in render.InstallInputs, bootDev string, plan diskPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth agent plan (windows apply-image) — task %s / machine %s\n", in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "MAMMOTH_WIN_MODE=%s\n", shQuote("apply"))
	fmt.Fprintf(&b, "MAMMOTH_WIN_WIM=%s\n", shQuote(in.Netboot.InstallWimURL))
	fmt.Fprintf(&b, "MAMMOTH_WIN_INDEX=%s\n", shQuote(fmt.Sprint(in.Netboot.InstallWimIndex)))
	fmt.Fprintf(&b, "MAMMOTH_COMPLETE_URL=%s\n", shQuote(in.CompleteURL))
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")
	fmt.Fprintf(&b, "MAMMOTH_WIN_UNATTEND=%s\n", shQuote(base+"/unattend-panther.xml"))
	fmt.Fprintf(&b, "MAMMOTH_WIN_TASKJSON=%s\n", shQuote(base+"/"+AgentTaskJSONName))
	// step-by-step progress channel: the VGA console freezes once /dev/console
	// lands on ttyS0 after the initramfs (real-machine finding, 9/22), so the
	// apply posts its own progress to the diag endpoint — the only live
	// observability during the silent phases.
	fmt.Fprintf(&b, "MAMMOTH_WIN_DIAG=%s\n", shQuote(base+"/diag"))
	fmt.Fprintf(&b, "MAMMOTH_WIN_SPECIALIZE=%s\n", shQuote(base+"/"+SpecializeXMLSeedName))

	b.WriteString("\n# disks: mammoth_disk <device> <size_bytes|->\n")
	for _, dsk := range in.Disks {
		// size rides along ONLY for controller-named volumes (Redfish
		// LogicalDriveN has no kernel node — the runtime resolves by size,
		// same mechanism as the kickstart %pre resolver).
		size := "-"
		if dsk.SizeBytes > 0 && !render.IsKernelDeviceName(dsk.Device) {
			size = fmt.Sprintf("%d", dsk.SizeBytes)
		}
		fmt.Fprintf(&b, "mammoth_disk %s %s\n", shQuote(dsk.Device), shQuote(size))
	}
	b.WriteString("\n# partitions: mammoth_partition <device> <mount> <fs> <size_mb|-> <flags> (final on-disk order; \"-\" fs = MSR, unformatted)\n")
	for i, p := range plan.partitions {
		mount, fs := "-", strings.ToLower(p.format)
		if p.kind == "MSR" {
			fs = "-"
		}
		if p.isOS {
			mount = "/"
		}
		if p.kind == "EFI" {
			mount = "/boot/efi"
			fs = "vfat"
		}
		size := fmt.Sprint(p.sizeMB)
		if p.extend {
			size = "-"
		}
		fmt.Fprintf(&b, "mammoth_partition %s %s %s %s %s\n",
			shQuote(bootDev), shQuote(mount), shQuote(fs), shQuote(size), shQuote(partFlags(i, plan)))
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
		gw := agent.DefaultGateway(n.Routes)
		if gw == "" {
			gw = "-"
		}
		ns := strings.Join(n.Nameservers, ",")
		if ns == "" {
			ns = "-"
		}
		fmt.Fprintf(&b, "mammoth_network %s %s %s %s\n", shQuote(mac), shQuote(addrs), shQuote(gw), shQuote(ns))
	}
	return b.String()
}

// partFlags renders the partition flags (the ESP's "esp" drives the GPT
// type GUID the runtime writes; the OS partition is the apply target).
func partFlags(i int, plan diskPlan) string {
	switch plan.partitions[i].kind {
	case "EFI":
		return "esp"
	default:
		return ""
	}
}

// winAgentPlanJSON renders the contract face of the windows plan (the
// documented shape a future agent implementation can consume directly).
func winAgentPlanJSON(d *Driver, in render.InstallInputs, bootDev string, plan diskPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n  \"version\": 1,\n  \"mode\": \"windows-apply\",\n  \"distro\": %q,\n  \"task_token\": %q,\n  \"machine_id\": %q,\n", d.Distro(), in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "  \"wim\": {\"url\": %q, \"index\": %d},\n", in.Netboot.InstallWimURL, in.Netboot.InstallWimIndex)
	fmt.Fprintf(&b, "  \"complete_url\": %q,\n", in.CompleteURL)
	b.WriteString("  \"disks\": [\n")
	for i, dsk := range in.Disks {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    {\"device\": %q, \"partitions\": [", dsk.Device)
		first := true
		for j, p := range plan.partitions {
			if dsk.Device != bootDev {
				continue // v1: only the boot disk carries partitions
			}
			if !first {
				b.WriteString(", ")
			}
			first = false
			mount, fs := "", strings.ToLower(p.format)
			if p.kind == "MSR" {
				fs = ""
			}
			if p.isOS {
				mount = "/"
			}
			if p.kind == "EFI" {
				mount = "/boot/efi"
				fs = "vfat"
			}
			size := p.sizeMB
			_ = j
			fmt.Fprintf(&b, "{\"fs\": %q, \"mount\": %q, \"size_mb\": %d, \"grow\": %t, \"flags\": [%s]}",
				fs, mount, size, p.extend, flagsJSON([]string{partFlags(j, plan)}))
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
			mac, cidrsJSON(n.Addresses), agent.DefaultGateway(n.Routes), strsJSON(n.Nameservers))
	}
	b.WriteString("\n  ]\n}\n")
	return b.String()
}

// flagsJSON renders a flag list as a JSON array body.
func flagsJSON(flags []string) string {
	var parts []string
	for _, f := range flags {
		if f != "" {
			parts = append(parts, strconv.Quote(f))
		}
	}
	return strings.Join(parts, ", ")
}

// cidrsJSON renders address strings as a JSON array body.
func cidrsJSON(addrs []string) string {
	var parts []string
	for _, a := range addrs {
		parts = append(parts, strconv.Quote(a))
	}
	return strings.Join(parts, ", ")
}

// strsJSON renders strings as a JSON array body.
func strsJSON(list []string) string {
	var parts []string
	for _, s := range list {
		parts = append(parts, strconv.Quote(s))
	}
	return strings.Join(parts, ", ")
}

// shQuote single-quotes a shell word (the plan faces are sourced by
// busybox sh — the same contract the agent plan renderer upholds).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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
	for i := range in.Disks {
		if &in.Disks[i] == boot {
			continue
		}
		if len(in.Disks[i].Partitions) > 0 {
			return diskPlan{}, fmt.Errorf("%s: disk %s declares partitions but is not the OS disk — v1 plans a single disk (secondary disks carry no partition spec)", distro, in.Disks[i].Device)
		}
	}

	plan := diskPlan{}
	espDone := false
	for i, p := range boot.Partitions {
		isESP := hasESP(p)
		isOS := p.Mount == "/"
		if isOS && plan.osIndex != 0 {
			return diskPlan{}, fmt.Errorf("%s: more than one partition mounts /", distro)
		}
		if !isESP && !isOS && p.Mount != "" {
			// Non-OS mounts are drive-letter territory — Windows assigns
			// letters itself; a declared mountpoint (swap included — it has
			// no meaning here) is a spec smell in v1.
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
            # Idempotent across the script's two executions: the address add
            # is a no-op when it exists, and the default route is rebuilt
            # AFTER the clear — a clear-then-add split across executions once
            # left the box with NO gateway (the second run removed the first
            # run's route; 2288H 9/22: same-subnet worked, routed sources
            # could not reach the machine at all).
            $first = $true
            foreach ($a in $n.ips) {
                New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress $a.ip -PrefixLength ([int]$a.prefix) -ErrorAction SilentlyContinue | Out-Null
                if ($first -and $n.gateway) {
                    Remove-NetRoute -DestinationPrefix "0.0.0.0/0" -Confirm:$false -ErrorAction SilentlyContinue
                    New-NetRoute -DestinationPrefix "0.0.0.0/0" -InterfaceIndex $nic.ifIndex -NextHop $n.gateway -RouteMetric 1 -ErrorAction SilentlyContinue | Out-Null
                }
                $first = $false
            }
            if ($n.dns) { Set-DnsClientServerAddress -InterfaceIndex $nic.ifIndex -ServerAddresses $n.dns }
        }
    }
    # --- user post_install segments: engine-managed orchestration. The
    # network binding above runs first and the completion callback below
    # runs LAST no matter what — a failing segment routes INTO the callback
    # as status=failed (the task surfaces INSTALL_FAILED, same face as the
    # Linux failtrap) and can never bypass it.
    $cbStatus = "ok"
    $cbDetail = "windows setup finished"
    $seg = @()
    if ($cfg.scripts -and $cfg.scripts.post_install) { $seg = @($cfg.scripts.post_install) }
    $marker = Join-Path $PSScriptRoot "mammoth-scripts.done"
    if ($seg.Count -gt 0) {
        if (Test-Path $marker) {
            # This script runs TWICE (SetupComplete in SYSTEM context and
            # again via FirstLogonCommands in the Administrator session —
            # whichever fires first on a given flow). Segments carry user
            # side effects: the second execution never re-runs them, it
            # replays the recorded verdict so both callback reports agree.
            $done = Get-Content $marker -Raw | ConvertFrom-Json
            $cbStatus = $done.status
            $cbDetail = $done.detail
        } else {
            for ($i = 1; $i -le $seg.Count; $i++) {
                $s = $seg[$i - 1]
                $shell = "cmd"
                if ($s.shell) { $shell = $s.shell }
                $ext = ".cmd"
                if ($shell -eq "powershell") { $ext = ".ps1" }
                $f = Join-Path $env:TEMP ("mammoth-post-" + $i + $ext)
                if ($s.content_base64) {
                    [IO.File]::WriteAllText($f, [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($s.content_base64)))
                } elseif ($s.url) {
                    Invoke-WebRequest -Uri $s.url -OutFile $f -UseBasicParsing -TimeoutSec 60
                }
                $code = -1
                if (Test-Path $f) {
                    if ($shell -eq "powershell") {
                        & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $f | Out-Null
                    } else {
                        & cmd.exe /d /c $f | Out-Null
                    }
                    $code = $LASTEXITCODE
                }
                $ok = $code -eq 0
                if ($s.expected_exit_codes -and @($s.expected_exit_codes).Count -gt 0) {
                    $ok = @($s.expected_exit_codes) -contains [int]$code
                }
                if (-not $ok) {
                    $cbStatus = "failed"
                    $cbDetail = "post_install script failed (segment $i/$($seg.Count), shell=$shell, exit=$code)"
                    break
                }
            }
            # Record BEFORE the callback: whatever happens later (callback
            # network dies, box reboots mid-retry), the second execution
            # replays this verdict instead of re-running the segments.
            Set-Content -Path $marker -Value (@{ status = $cbStatus; detail = $cbDetail } | ConvertTo-Json -Compress)
        }
    }
    # Completion callback — single-shot POST is timing-sensitive right after
    # image apply (the 2288H round's first-boot callback silently died
    # there) — retry for up to ~75s until the engine acks one of them
    # (duplicate callbacks are harmless, the engine keeps the first terminal
    # report).
    $body = @{ status = $cbStatus; detail = $cbDetail } | ConvertTo-Json -Compress
    foreach ($i in 1..6) {
        try {
            Invoke-WebRequest -Uri $cfg.complete_url -Method POST -Body $body -ContentType 'application/json' -UseBasicParsing -TimeoutSec 15 | Out-Null
            break
        } catch { Start-Sleep -Seconds 12 }
    }
}
# AutoLogon(once) leaves the plaintext credential in Winlogon — scrub it.
# CONTEXT MATTERS: this script runs TWICE — as SetupComplete (SYSTEM, pre
# logon, windeploy's RunUserProvidedScript — the agent apply pathway's
# SetupComplete DOES execute, unlike the setup.exe flow's) and again as
# FirstLogonCommands (the Administrator session). The scrub MUST only run
# in the user context: removing the AutoLogon values from the SYSTEM
# context races winlogon's own auto-logon execution and hangs the boot
# with a dead console on the VGA (2288H 9/22, three rounds).
if (-not [Security.Principal.WindowsIdentity]::GetCurrent().IsSystem) {
    Remove-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" -Name DefaultPassword -ErrorAction SilentlyContinue
    Remove-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" -Name AutoAdminLogon -ErrorAction SilentlyContinue
    Remove-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" -Name AutoLogonCount -ErrorAction SilentlyContinue
}
# Optional remote management (access.remote_management, DEFAULT OFF —
# opening inbound RDP/WinRM/ICMP on a fresh install is the operator's
# security call). The iBMC console on some hardware renders the Core
# session as a dead, input-deaf window (2288H 9/22); when opted in, the
# operator face becomes the network instead.
$rm = @($cfg.capabilities)
# Enable-NetFirewallRule only flips EXISTING rule instances, and the
# default RDP/ICMP rules often exist for Domain/Private profiles only —
# a fresh NIC lands on the PUBLIC profile (no domain), where nothing is
# enabled and routed sources stay blocked (2288H 9/22: same-subnet worked,
# routed operator subnet did not). Explicit Any-profile rules close that
# gap; -ErrorAction SilentlyContinue keeps the double execution idempotent.
if ($rm -contains "rdp") {
    Set-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Terminal Server" -Name fDenyTSConnections -Value 0
    Enable-NetFirewallRule -Name RemoteDesktop-UserMode-In-TCP,RemoteDesktop-UserMode-In-UDP -ErrorAction SilentlyContinue
    Set-NetFirewallRule -Name RemoteDesktop-UserMode-In-TCP -RemoteAddress Any -ErrorAction SilentlyContinue
    New-NetFirewallRule -Name "mammoth-rdp-in" -DisplayName "mammoth RDP" -Profile Any -Direction Inbound -Protocol TCP -LocalPort 3389 -Action Allow -RemoteAddress Any -ErrorAction SilentlyContinue | Out-Null
}
if ($rm -contains "winrm") { Enable-PSRemoting -Force -SkipNetworkProfileCheck -ErrorAction SilentlyContinue | Out-Null }
if ($rm -contains "ping") {
    Enable-NetFirewallRule -Name FPS-ICMP4-ERQ-In,FPS-ICMP6-ERQ-In -ErrorAction SilentlyContinue
    New-NetFirewallRule -Name "mammoth-ping-in" -DisplayName "mammoth ping" -Profile Any -Direction Inbound -Protocol ICMPv4 -Action Allow -RemoteAddress Any -ErrorAction SilentlyContinue | Out-Null
}
`
}

// SetupCompleteSeedPair renders the GENERIC SetupComplete pair under the
// prepared-tree injection contract's names. The builder injects this pair
// into install.wim (C:\Windows\Setup\Scripts\) — the running OS's first
// boot consumes it in BOTH pathways (setup.exe flow via FirstLogon
// fallback, agent flow via FirstLogonCommands), and the task-specific
// half (task.json) deliberately rides separately so the pair stays
// ISO-sha cacheable. provision feeds this into EnsureWindowsTree /
// BuildWindowsWimboot wherever the render answers themselves do not carry
// the pair (the agent apply pathway's answer set never did — a latent gap
// the v4 cache bust exposed as INSTALL_MEDIA_BUILD_FAILED, 9/22).
// The pair stays task-independent (capabilities ride task.json at
// runtime), so the cached prepared tree is unaffected by the opt-in.
func SetupCompleteSeedPair() map[string]string {
	return map[string]string{
		SetupCompleteSeedName: setupCompleteCmd(),
		CompletePS1SeedName:   mammothCompletePS(),
	}
}

// taskConfig is the per-task runtime contract consumed at first boot.
type taskConfig struct {
	CompleteURL string       `json:"complete_url"`
	Network     []taskNetNIC `json:"network,omitempty"`
	// Capabilities mirrors access.capabilities ("rdp"|"winrm"|"ping"):
	// the access features the ps1 enables on the installed system.
	Capabilities []string `json:"capabilities,omitempty"`
	// Scripts is nil unless the spec declared post_install segments — no
	// scripts means a task.json byte-identical to the pre-scripts shape.
	Scripts *taskScripts `json:"scripts,omitempty"`
}

// taskScript is one user post_install segment as the first-boot script sees
// it (docs/04-install-spec.md §5): inline bodies re-encode base64 so
// task.json stays ASCII-safe, URL bodies are fetched at runtime.
type taskScript struct {
	Shell         string `json:"shell"`
	ContentBase64 string `json:"content_base64,omitempty"`
	URL           string `json:"url,omitempty"`
	ExpectedExit  []int  `json:"expected_exit_codes,omitempty"`
}

type taskScripts struct {
	PostInstall []taskScript `json:"post_install,omitempty"`
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

// taskJSON renders the per-task config (ISO root — the only per-task seed on
// the setup pathway; the agent apply pathway injects it in-wim pre-apply).
func taskJSON(completeURL string, entries []render.NetworkEntry, capabilities []string, post []render.ScriptEntry) (string, error) {
	cfg := taskConfig{CompleteURL: completeURL, Capabilities: capabilities}
	if len(post) > 0 {
		ts := &taskScripts{}
		for _, s := range post {
			ts.PostInstall = append(ts.PostInstall, taskScript{
				Shell:         scriptShell(s),
				ContentBase64: base64.StdEncoding.EncodeToString([]byte(s.Inline)),
				URL:           s.URL,
				ExpectedExit:  s.ExpectedExit,
			})
		}
		cfg.Scripts = ts
	}
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
// preSyncCommands renders the windowsPE RunSynchronous Path lines: each
// pre_install segment rides the boot medium as mammoth/pre-<n>.cmd and the
// command line locates it across the WinPE drive letters (the medium's
// letter differs between carriers and machines). The script's exit code
// propagates as the command's — a non-zero aborts setup with no callback,
// surfacing as INSTALL_TIMEOUT (v1; documented in docs/04-install-spec.md).
func preSyncCommands(n int) []string {
	cmds := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		cmds = append(cmds, fmt.Sprintf(`cmd /d /s /c "for %%i in (C D E F G X Y Z) do @if exist %%i:\mammoth\pre-%d.cmd %%i:\mammoth\pre-%d.cmd"`, i, i))
	}
	return cmds
}

func unattendXML(imageName, hostname, password string, ml mediaLocale, plan diskPlan, preCmds []string) string {
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
`)
	// Schema children of Microsoft-Windows-Setup are order-sensitive
	// (alphabetical): RunSynchronous slots between ImageInstall and
	// UserData. RunSynchronous is the native multi-slot WinPE seat — it
	// runs during the windowsPE pass, before the image is applied.
	if len(preCmds) > 0 {
		w(`      <RunSynchronous>
`)
		for i, c := range preCmds {
			w(fmt.Sprintf(`        <RunSynchronousCommand wcm:action="add">
          <Order>%d</Order>
          <Description>mammoth pre_install script %d</Description>
          <Path>%s</Path>
        </RunSynchronousCommand>
`, i+1, i+1, xmlEscape(c)))
		}
		w(`      </RunSynchronous>
`)
	}
	w(`      <!-- ProductKey: the element must exist (setup aborts reading the
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
` + postSetupPassesXML(hostname, password, ml) + `</unattend>
`)
	return b.String()
}

// postSetupPassesXML renders the specialize + oobeSystem passes â the OS
// runs these on first boot no matter which pathway applied the image
// (setup.exe DiskConfiguration or the agent wimlib apply), so the
// autounattend and the Panther variant share this exact XML source.
// AutoLogon(once) + FirstLogonCommands are the PRIMARY completion trigger
// â both native Shell-Setup oobeSystem settings and PROVEN to run on this
// path (AdministratorPassword took effect), unlike SetupComplete whose
// auto-execution silently never fired (setupact has zero records, root
// cause open). RunSynchronous was tried first and rejected: Shell-Setup
// has no such element in oobeSystem (setup aborts the pass with
// "component or setting does not exist"). The ps1 is idempotent â it
// configures the declared static network then POSTs the callback.
func postSetupPassesXML(hostname, password string, ml mediaLocale) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s) }
	w(`  <settings pass="specialize">
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
      <AutoLogon>
        <Enabled>true</Enabled>
        <LogonCount>1</LogonCount>
        <Username>Administrator</Username>
        <Password>
          <Value>` + xmlEscape(password) + `</Value>
          <PlainText>true</PlainText>
        </Password>
      </AutoLogon>
      <FirstLogonCommands>
        <SynchronousCommand wcm:action="add">
          <Order>1</Order>
          <CommandLine>powershell -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Setup\Scripts\mammoth-complete.ps1</CommandLine>
          <Description>mammoth static network + completion callback</Description>
        </SynchronousCommand>
      </FirstLogonCommands>
      <!-- Element order inside a component is the schema sequence
           (alphabetical): AutoLogon -> FirstLogonCommands -> OOBE ->
           UserAccounts. A mis-ordered element fails the WHOLE unattend at
           the earliest pass (offlineServicing: "cannot apply unattend
           settings") â not the pass it belongs to. -->
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
`)
	return b.String()
}

// pantherUnattendXML renders the apply-image unattend: specialize +
// oobeSystem only, NO windowsPE pass â DiskConfiguration, ImageInstall and
// International-WinPE are setup.exeï¼s job, and the agent pathway replaces
// setup with wimlib apply + the pre-baked BCD. The OS consumes this file
// from %WINDIR%\\Panther\\unattend.xml on first boot (the canonical implicit
// path for the running system; drive-root autounattend discovery is a
// setup.exe behavior that does not exist here).
func pantherUnattendXML(hostname, password string, ml mediaLocale) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
` + postSetupPassesXML(hostname, password, ml) + `</unattend>
`
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

// bcdBootStartnetCmd renders the boot-two startnet (方案 A, WinPE bcdboot
// 收尾): after the agent laid the image down, a wimboot WinPE generates the
// boot store NATIVELY — the exact timing setup.exe itself uses (bcdboot in
// WinPE, then reboot into specialize). Baked into boot.wim as the wimboot
// startnet seed; never fetched by name.
//
// Orchestration contract: the script POSTs its verdict marker to the diag
// channel, then HOLDS — it must never reboot on its own, because the engine
// has to release the PXE entries BEFORE the machine resets (a self-reboot
// races the release and re-enters the installer); the engine power-cycles
// on the marker instead. Dynamic values: only the diag URL, restricted to
// the machine-face scheme (scheme://host:port/render/<hextoken>/diag) —
// cmd cannot quote metacharacters, the same restricted-charset contract
// the setup startnet's URLs uphold.
func bcdBootStartnetCmd(diagURL string) string {
	var b strings.Builder
	w := func(line string) { b.WriteString(line); b.WriteByte('\n') }
	progress := func(msg string) {
		w(fmt.Sprintf("curl.exe -sf -m 20 -X POST --data-binary \"%s\" %s/win-progress >nul 2>&1", msg, diagURL))
	}
	w("@echo off")
	w("rem mammoth boot two: generate the BCD natively with bcdboot - the")
	w("rem setup.exe timing. The agent left the ESP empty ON PURPOSE: a")
	w("rem hand-patched store never passes NT's BcdOpenStore (0xC0000098).")
	progress("boot two: bcdboot stage starting (wpeinit)")
	w("wpeinit")
	// The ESP is partition 1 of disk 0: the agent carved ESP/MSR/NTFS in
	// that order, and this flow is single-OS-disk by contract. The spaced
	// redirect form is load-bearing — `echo ... 0>file` / `1>>file` parse
	// as handle redirects and silently drop the digit.
	w("echo select disk 0 > X:\\mammoth-dp.txt")
	w("echo select partition 1 >> X:\\mammoth-dp.txt")
	w("echo assign letter=S >> X:\\mammoth-dp.txt")
	w("diskpart /s X:\\mammoth-dp.txt > X:\\mammoth-diskpart.log 2>&1")
	w("set MRK=X:\\mammoth-marker.txt")
	w(`if exist S:\ goto havesp`)
	w("echo " + BCDBootMarkerFail + " no-esp-letter > %MRK%")
	w("type X:\\mammoth-diskpart.log >> %MRK%")
	w(fmt.Sprintf("curl.exe -sf -m 30 -X POST --data-binary @%%MRK%% %s/%s >nul 2>&1", diagURL, BCDBootDiagMarker))
	w("goto hold")
	w(":havesp")
	// The applied NTFS volume gets whatever letter WinPE hands out — find
	// it by its content instead of guessing (the task.json copy loop in
	// the setup startnet is the same idiom).
	w("set WDRV=")
	w("for %%d in (C D E F W) do if exist %%d:\\Windows\\System32 set WDRV=%%d")
	w(`if "%WDRV%"=="" (`)
	w("  echo " + BCDBootMarkerFail + " no-windows-volume > %MRK%")
	w(fmt.Sprintf("  curl.exe -sf -m 30 -X POST --data-binary @%%MRK%% %s/%s >nul 2>&1", diagURL, BCDBootDiagMarker))
	w("  goto hold")
	w(")")
	w(fmt.Sprintf("bcdboot %%WDRV%%:\\Windows /s S: /f UEFI > X:\\mammoth-bcdboot.log 2>&1"))
	w("set RC=%errorlevel%")
	// The acceptance gate (external review's standing check): the generated
	// store must OPEN under the NT BCD layer — bcdedit is exactly that
	// layer; 0xC0000098 here means the store is as dead as our hand-made
	// ones were.
	w("bcdedit /store S:\\EFI\\Microsoft\\Boot\\BCD /enum all > X:\\mammoth-bcdedit.log 2>&1")
	w("set RC2=%errorlevel%")
	w(`if not "%RC%"=="0" goto bcdfail`)
	w(`if not "%RC2%"=="0" goto bcdfail`)
	w("echo " + BCDBootMarkerOK + " > %MRK%")
	w("goto emit")
	w(":bcdfail")
	w("echo " + BCDBootMarkerFail + " bcdboot-rc=%RC% bcdedit-rc=%RC2% > %MRK%")
	w(":emit")
	w("type X:\\mammoth-bcdboot.log >> %MRK%")
	w("echo --- bcdedit /enum all (NT BcdOpenStore gate) --- >> %MRK%")
	w("type X:\\mammoth-bcdedit.log >> %MRK%")
	// Firmware-fallback insurance: bcdboot /f UEFI writes the canonical
	// EFI\Microsoft path and registers the NVRAM entry; the removable
	// fallback doubles the road in if the NVRAM write or lookup fails.
	w(`if exist S:\EFI\Microsoft\Boot\bootmgfw.efi (`)
	w("  mkdir S:\\EFI\\Boot >nul 2>&1")
	w("  copy /Y S:\\EFI\\Microsoft\\Boot\\bootmgfw.efi S:\\EFI\\Boot\\bootx64.efi >nul 2>&1")
	w(")")
	w(fmt.Sprintf("curl.exe -sf -m 30 -X POST --data-binary @%%MRK%% %s/%s >nul 2>&1", diagURL, BCDBootDiagMarker))
	w(":hold")
	// HOLD forever: the engine releases PXE + power-cycles on the marker.
	w("ping -n 11 127.0.0.1 >nul")
	w("goto hold")
	return b.String()
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
