package windows

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func baseInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken: "tokw", MachineID: "mch_w", Hostname: "node-w1",
		ImageSource:   "file:///data/os_iso/windows/server2019.iso",
		RootPassword:  "wRoot-pw",
		AnswerBaseURL: "http://10.0.2.2:8080/render/tokw",
		CompleteURL:   "http://10.0.2.2:8080/render/tokw/complete",
		Disks: []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
			{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
			{Mount: "/", FS: "ntfs", Grow: true},
		}}},
		Network: []render.NetworkEntry{{
			Match:       &render.NetMatch{MAC: "aa:bb:cc:dd:ee:0a"},
			Addresses:   []string{"172.16.1.50/24", "172.16.1.51/24"},
			Routes:      []render.NetRoute{{To: "default", Via: "172.16.1.1"}},
			Nameservers: []string{"10.0.0.53", "10.0.0.54"},
		}},
	}
}

// The v1 unattend golden: UEFI GPT shape (ESP + mammoth MSR + Windows),
// Standard Core SKU selection by /IMAGE/NAME, callback + static network in
// SetupComplete.cmd (SYSTEM, pre-logon — the Windows late-commands).
func TestRenderWindowsUEFIUnattend(t *testing.T) {
	answers, boot, err := New("windows2019").RenderAnswers(baseInputs(), render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(answers) != 4 {
		t.Fatalf("answers = %d, want autounattend + SetupComplete pair + task.json", len(answers))
	}
	var unattend, setup, ps1, task string
	for _, a := range answers {
		switch a.Name {
		case "autounattend.xml":
			unattend = a.Content
		case SetupCompleteSeedName:
			setup = a.Content
		case CompletePS1SeedName:
			ps1 = a.Content
		case TaskJSONSeedName:
			task = a.Content
		}
	}
	if unattend == "" || setup == "" || ps1 == "" || task == "" {
		t.Fatalf("missing answer files: %+v", answers)
	}

	// unattend: SKU by image name, UEFI layout with the MSR inserted after
	// the ESP, InstallTo = first position after ESP+MSR, identity + OOBE.
	for _, want := range []string{
		`<Value>Windows Server 2019 SERVERSTANDARDCORE</Value>`,
		`<Type>EFI</Type>`, `<Size>512</Size>`,
		`<Type>MSR</Type>`, `<Size>16</Size>`,
		`<Type>Primary</Type>`, `<Extend>true</Extend>`,
		`<Format>FAT32</Format>`, `<Format>NTFS</Format>`,
		`<PartitionID>3</PartitionID>`,
		`<DiskID>0</DiskID>`, `<WillWipeDisk>true</WillWipeDisk>`,
		`<ComputerName>node-w1</ComputerName>`,
		`<Value>wRoot-pw</Value>`, `<PlainText>true</PlainText>`,
		`<AcceptEula>true</AcceptEula>`,
		// ProductKey element must exist but the Key stays empty: a key here
		// (even the public KMS client setup key) sends setup down the
		// key-validation path, which fails in the network launch shape and
		// pops the product-key page (qemu 9/21); the empty-Key contract
		// resolves via the builder's sources/ei.cfg (Volume channel).
		`<ProductKey>`,
		`<Key></Key>`,
		`<HideEULAPage>true</HideEULAPage>`,
		// oobeSystem RunSynchronous is the PRIMARY completion trigger —
		// proven to run on the real machine (AdministratorPassword took
		// effect); SetupComplete's auto-execution did not fire there.
		// FirstLogonCommands(+ AutoLogon once) is the PRIMARY completion
		// trigger: native Shell-Setup oobeSystem settings, proven to run
		// (AdministratorPassword took effect). RunSynchronous is NOT valid
		// in Shell-Setup — setup aborts the pass ("component or setting
		// does not exist", 2288H 9/21). SetupComplete stays as backup.
		`<CommandLine>powershell -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Setup\Scripts\mammoth-complete.ps1</CommandLine>`,
		// The windowsPE international component must be International-
		// Core-WinPE: the "International-WinPE" short name parses fine but
		// SMI rejects it wholesale, setup/target language stay undetermined
		// and setup shows the language-selection page (real-media setupact,
		// 9/21). Values are the media's own language — the zh-CN media has
		// no en-US setup resources, so en-US settings fall to the same page.
		`name="Microsoft-Windows-International-Core-WinPE"`,
		`<UILanguage>zh-CN</UILanguage>`,
		`<InputLocale>0804:00000804</InputLocale>`,
	} {
		if !strings.Contains(unattend, want) {
			t.Errorf("autounattend.xml missing %q", want)
		}
	}
	if strings.Contains(unattend, `name="Microsoft-Windows-International-WinPE"`) {
		t.Errorf("autounattend.xml uses the non-existent International-WinPE component (SMI rejects it, language page returns)")
	}
	if strings.Contains(unattend, "en-US") {
		t.Errorf("autounattend.xml carries en-US settings — the zh-CN media has no en-US setup resources")
	}
	if strings.Contains(unattend, "wRoot-pw</Value>") && !strings.Contains(unattend, "PlainText>true") {
		t.Errorf("password present without plaintext declaration")
	}

	// boot params: setup needs no kernel arguments and reboots itself.
	if boot.KernelArgs != "" {
		t.Errorf("kernel args must be empty, got %q", boot.KernelArgs)
	}
	if !boot.InstallerAutoReboot {
		t.Errorf("installer auto reboot must be declared")
	}
	if !strings.HasSuffix(boot.AnswerURL, "/render/tokw/autounattend.xml") {
		t.Errorf("answer url wrong: %q", boot.AnswerURL)
	}

	// SetupComplete pair is generic (sha-cacheable wim injection); the
	// per-task contract lives in task.json — NIC by normalized MAC,
	// primary ip carries the gateway, secondaries ride along, DNS on top.
	if !strings.Contains(setup, "mammoth-complete.ps1") {
		t.Errorf("SetupComplete.cmd must launch the generic ps1:\n%s", setup)
	}
	if strings.Contains(setup, "10.0.2.2") {
		t.Errorf("per-task URL leaked into the generic launcher (breaks wim caching)")
	}
	for _, want := range []string{
		`mammoth\task.json`, `Get-NetAdapter`, `New-NetIPAddress`,
		`Set-DnsClientServerAddress`, `$cfg.complete_url`,
		// static config clears a conflicting DHCP default route first
		`Remove-NetRoute -DestinationPrefix "0.0.0.0/0"`,
		// the AutoLogon scrub is user-context-only: the SetupComplete
		// (SYSTEM) execution must not race winlogon's auto-logon
		`GetCurrent().IsSystem`,
		`Remove-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon" -Name AutoAdminLogon`,
	} {
		if !strings.Contains(ps1, want) {
			t.Errorf("mammoth-complete.ps1 missing %q", want)
		}
	}
	for _, want := range []string{
		`"complete_url":"http://10.0.2.2:8080/render/tokw/complete"`,
		`"mac":"AA-BB-CC-DD-EE-0A"`,
		`"ip":"172.16.1.50","prefix":"24"`,
		`"gateway":"172.16.1.1"`,
		`"ip":"172.16.1.51","prefix":"24"`,
		`"dns":["10.0.0.53","10.0.0.54"]`,
	} {
		if !strings.Contains(task, want) {
			t.Errorf("task.json missing %q:\n%s", want, task)
		}
	}
}

// No ESP declared: the UEFI layout is synthesized (300 MiB EFI + MSR ahead
// of everything, mirroring NormalizeESP's auto-flag for the Linux dialects).
func TestRenderWindowsSynthesizesESP(t *testing.T) {
	in := baseInputs()
	in.Disks = []render.ResolvedDisk{{Device: "sda", Wipe: true, Partitions: []render.ResolvedPartition{
		{Mount: "/", FS: "ntfs", SizeMB: 40960},
	}}}
	answers, _, err := New("windows2019").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var unattend string
	for _, a := range answers {
		if a.Name == "autounattend.xml" {
			unattend = a.Content
		}
	}
	if !strings.Contains(unattend, "<Size>300</Size>") {
		t.Errorf("synthesized ESP (300 MiB) missing")
	}
	if !strings.Contains(unattend, "<PartitionID>3</PartitionID>") {
		t.Errorf("InstallTo must land after ESP+MSR")
	}
	if !strings.Contains(unattend, "<Size>40960</Size>") {
		t.Errorf("declared OS size missing")
	}
}

// The boundary, enforced at render time (honest rejections, not silent
// reinterpretation) and via capability declarations at submit time. The
// wimboot carrier is full: the >4G boot.wim dead end was replaced by the
// deployment SMB export mapped from a baked startnet, and the gate rejects
// submissions when that export is not configured.
func TestRenderWindowsBoundary(t *testing.T) {
	d := New("windows2019")
	if d.PXESupport() != render.SupportFull {
		t.Errorf("windows PXE support must be full (wimboot carrier + deployment SMB export)")
	}
	if d.NetbootCarrier() != render.NetbootCarrierWimboot {
		t.Errorf("windows netboot carrier must be wimboot")
	}
	if d.NetbootPool() != render.NetbootPoolNone {
		t.Errorf("windows netboot pool must be none (install source rides the SMB export)")
	}
	if d.KeepPartitionSupport() != render.SupportNone {
		t.Errorf("windows keep support must be none in v1")
	}
	if d.FirmwareSupport() != render.FirmwareUEFIOnly {
		t.Errorf("windows unattend is UEFI-shaped — firmware support must say so")
	}
	if got := d.SupportedArchs(); len(got) != 1 || got[0] != render.ArchAMD64 {
		t.Errorf("archs = %v, want [amd64]", got)
	}

	cases := []struct {
		name    string
		mutate  func(in render.InstallInputs) render.InstallInputs
		wantErr string
	}{
		{"pxe input with pool", func(in render.InstallInputs) render.InstallInputs {
			in.Netboot = &render.NetbootInputs{PoolURL: "http://x/store/s/iso", NFSRootURL: "h:/p"}
			return in
		}, "no pool tree applies"},
		{"pxe without share", func(in render.InstallInputs) render.InstallInputs {
			in.Netboot = &render.NetbootInputs{}
			return in
		}, "needs the deployment SMB export"},
		{"pxe share metachar", func(in render.InstallInputs) render.InstallInputs {
			in.Netboot = &render.NetbootInputs{InstallSMBUNC: `\\h\share`, InstallSMBPassword: "p&ss"}
			return in
		}, "cmd cannot quote metacharacters"},
		{"raid", func(in render.InstallInputs) render.InstallInputs {
			in.Raid = []render.ResolvedRaid{{Name: "v0", Mode: "hardware"}}
			return in
		}, "RAID is not supported"},
		{"user scripts", func(in render.InstallInputs) render.InstallInputs {
			in.Scripts = []render.ScriptEntry{{Stage: "post_install", Inline: "echo hi"}}
			return in
		}, "user scripts are not supported"},
		{"bond", func(in render.InstallInputs) render.InstallInputs {
			in.Network = []render.NetworkEntry{{Bond: &render.NetBond{Mode: "802.3ad"},
				Match: &render.NetMatch{MAC: "aa:bb:cc:dd:ee:0a"}}}
			return in
		}, "bond/vlan is not supported"},
		{"no mac match", func(in render.InstallInputs) render.InstallInputs {
			in.Network = []render.NetworkEntry{{Addresses: []string{"10.0.0.5/24"}}}
			return in
		}, "must match a MAC"},
		{"extra route", func(in render.InstallInputs) render.InstallInputs {
			in.Network[0].Routes = append(in.Network[0].Routes, render.NetRoute{To: "10.20.0.0/16", Via: "172.16.1.254"})
			return in
		}, "non-default route"},
		{"non-windows fs", func(in render.InstallInputs) render.InstallInputs {
			in.Disks[0].Partitions[1].FS = "xfs"
			return in
		}, "not a windows filesystem"},
		{"grow not last", func(in render.InstallInputs) render.InstallInputs {
			in.Disks[0].Partitions = []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ntfs", Grow: true},
				{Mount: "", FS: "ntfs", SizeMB: 1024},
			}
			return in
		}, "grow is only allowed on the last partition"},
		{"no os partition", func(in render.InstallInputs) render.InstallInputs {
			in.Disks[0].Partitions = in.Disks[0].Partitions[:1]
			return in
		}, "no OS partition"},
		{"ssh keys", func(in render.InstallInputs) render.InstallInputs {
			in.SSHPublicKeys = []string{"ssh-ed25519 AAA"}
			return in
		}, "access.ssh_keys is not supported"},
		{"second disk partitions", func(in render.InstallInputs) render.InstallInputs {
			in.Disks = append(in.Disks, render.ResolvedDisk{Device: "sdb", Wipe: true,
				Partitions: []render.ResolvedPartition{{FS: "ntfs", SizeMB: 10240}}})
			return in
		}, "is not the OS disk"},
		{"swap mount", func(in render.InstallInputs) render.InstallInputs {
			in.Disks[0].Partitions = []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ntfs", SizeMB: 40960},
				{Mount: "swap", SizeMB: 4096},
			}
			return in
		}, `mount "swap" is not supported`},
		{"bad hostname", func(in render.InstallInputs) render.InstallInputs {
			in.Hostname = "node/illegal"
			return in
		}, "not valid in a Windows computer name"},
		{"hostname truncated to 15", func(in render.InstallInputs) render.InstallInputs {
			in.Hostname = "very-long-hostname-over-limit"
			return in
		}, ""},
	}
	for _, tc := range cases {
		_, _, err := d.RenderAnswers(tc.mutate(baseInputs()), render.MachineView{})
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}

// Guest-share mode (no credentials): the startnet opts the WinPE SMB client
// into insecure guest sessions before mapping.
func TestRenderWindowsPXEGuestShare(t *testing.T) {
	in := baseInputs()
	in.Netboot = &render.NetbootInputs{
		InstallSMBUNC:       `\\198.51.100.248\mammoth-media`,
		InstallSMBImagePath: `pool-store\abc123\win\tree`,
	}
	answers, _, err := New("windows2019").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var startnet string
	for _, a := range answers {
		if a.Name == "mammoth/startnet.cmd" {
			startnet = a.Content
		}
	}
	for _, want := range []string{
		`AllowInsecureGuestAuth /t REG_DWORD /d 1 /f`,
		`net use Z: \\198.51.100.248\mammoth-media "" /user:guest`,
	} {
		if !strings.Contains(startnet, want) {
			t.Errorf("guest startnet missing %q:\n%s", want, startnet)
		}
	}
}

// The wimboot PXE shape renders the virtual-media answers plus the startnet
// that maps the deployment SMB export (the install source), and carries no
// kernel args.
func TestRenderWindowsPXE(t *testing.T) {
	in := baseInputs()
	in.Netboot = &render.NetbootInputs{
		InstallSMBUNC:       `\\198.51.100.248\mammoth-media`,
		InstallSMBUser:      "smbuser",
		InstallSMBPassword:  "s3cret-9",
		InstallSMBImagePath: `pool-store\abc123\win\tree`,
	}
	answers, boot, err := New("windows2019").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	names := map[string]string{}
	for _, a := range answers {
		names[a.Name] = a.Content
	}
	for _, want := range []string{"autounattend.xml", "mammoth/SetupComplete.cmd", "mammoth/mammoth-complete.ps1", "mammoth/task.json", "mammoth/startnet.cmd"} {
		if _, ok := names[want]; !ok {
			t.Errorf("PXE render missing answer %s (the wimboot seed contract)", want)
		}
	}
	startnet := names["mammoth/startnet.cmd"]
	for _, want := range []string{
		"wpeinit",
		`net use Z: \\198.51.100.248\mammoth-media "s3cret-9" /user:smbuser`,
		`start "mammoth setup" /D Z:\pool-store\abc123\win\tree Z:\pool-store\abc123\win\tree\sources\setup.exe`,
		// diag uploads are POST (--data-binary): the machine endpoint has no
		// PUT route, and -T (PUT) failed silently under -sf every round.
		`curl -sf -X POST --data-binary @X:\Windows\Panther\setuperr.log http://10.0.2.2:8080/render/tokw/diag/setuperr.log`,
		// The diag loop ships task.json onto the applied volume: the wimboot
		// ramdisk (X:) dies at setup auto-reboot, and SetupComplete reads
		// its config from this same directory (2288H round, 9/21).
		`for %%d in (C D E F) do if exist %%d:\Windows\Setup\Scripts\mammoth-complete.ps1 copy /Y X:\mammoth\task.json %%d:\Windows\Setup\Scripts\task.json >nul 2>&1`,
	} {
		if !strings.Contains(startnet, want) {
			t.Errorf("startnet missing %q:\n%s", want, startnet)
		}
	}
	if boot.KernelArgs != "" || boot.NetbootKernelArgs != "" {
		t.Errorf("wimboot entries carry no kernel args, got %q / %q", boot.KernelArgs, boot.NetbootKernelArgs)
	}
	if !boot.InstallerAutoReboot {
		t.Errorf("windows setup reboots itself — InstallerAutoReboot must hold on PXE too")
	}
}

// Long hostnames truncate to the 15-char NetBIOS label instead of failing —
// the truncation is deterministic and worth pinning.
func TestComputerNameTruncation(t *testing.T) {
	name, err := computerName("very-long-hostname-over-limit")
	if err != nil {
		t.Fatalf("computerName: %v", err)
	}
	if name != "very-long-hostn" {
		t.Errorf("computerName = %q, want very-long-hostn", name)
	}
}

// The unattend language set follows the detected media language; unknown
// or empty tokens fall back to the driver default (the registered media's
// language), never to a guessed value the media may not carry.
func TestMediaLocaleResolution(t *testing.T) {
	for _, tc := range []struct {
		lang string
		want mediaLocale
	}{
		{"zh-cn", mediaLocale{uiLang: "zh-CN", inputLocale: "0804:00000804"}},
		{"en-us", mediaLocale{uiLang: "en-US", inputLocale: "0409:00000409"}},
		{"  EN-US ", mediaLocale{uiLang: "en-US", inputLocale: "0409:00000409"}},
		{"fr-fr", defaultMediaLocale},
		{"", defaultMediaLocale},
	} {
		if got := mediaLocaleFor(tc.lang); got != tc.want {
			t.Errorf("mediaLocaleFor(%q) = %+v, want %+v", tc.lang, got, tc.want)
		}
	}
}

// An en-US media token must flow through to the unattend verbatim — the
// round trip detection → render is the whole point of the contract.
func TestRenderWindowsEnUSMedia(t *testing.T) {
	in := baseInputs()
	in.MediaLanguage = "en-us"
	answers, _, err := New("windows2019").RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var unattend string
	for _, a := range answers {
		if a.Name == "autounattend.xml" {
			unattend = a.Content
		}
	}
	for _, want := range []string{
		`<UILanguage>en-US</UILanguage>`,
		`<InputLocale>0409:00000409</InputLocale>`,
		`<SystemLocale>en-US</SystemLocale>`,
		`<UserLocale>en-US</UserLocale>`,
	} {
		if !strings.Contains(unattend, want) {
			t.Errorf("autounattend.xml missing %q", want)
		}
	}
	if strings.Contains(unattend, "zh-CN") || strings.Contains(unattend, "0804") {
		t.Errorf("en-US media round fell back to zh-CN settings:\n%s", unattend)
	}
}

// access.capabilities (opt-in): channels ride task.json into the ps1, which
// opens only the requested firewall groups; unknown channels are rejected.
func TestRenderWindowsCapabilities(t *testing.T) {
	in := baseInputs()
	in.Capabilities = []string{"rdp", "ping"}
	_, _, ps1Task, err := func() (string, string, string, error) {
		ans, _, err := New("windows2019").RenderAnswers(in, render.MachineView{})
		if err != nil {
			return "", "", "", err
		}
		for _, a := range ans {
			if a.Name == TaskJSONSeedName {
				return "", "", a.Content, nil
			}
			if a.Name == AgentTaskJSONName {
				return "", "", a.Content, nil
			}
		}
		return "", "", "", nil
	}()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{`"capabilities":["rdp","ping"]`, `"rdp"`, `"ping"`} {
		if !strings.Contains(ps1Task, want) {
			t.Errorf("task.json missing %q", want)
		}
	}
	in2 := baseInputs()
	in2.Capabilities = []string{"telnet"}
	if _, _, err := New("windows2019").RenderAnswers(in2, render.MachineView{}); err == nil {
		t.Errorf("unknown capability accepted")
	}
}
