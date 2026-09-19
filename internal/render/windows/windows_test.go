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
		`<HideEULAPage>true</HideEULAPage>`,
	} {
		if !strings.Contains(unattend, want) {
			t.Errorf("autounattend.xml missing %q", want)
		}
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

// The v1 boundary, enforced at render time (honest rejections, not silent
// reinterpretation) and via capability declarations at submit time.
func TestRenderWindowsBoundary(t *testing.T) {
	d := New("windows2019")
	if d.PXESupport() != render.SupportNone {
		t.Errorf("windows PXE support must be none in v1")
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
		{"pxe input", func(in render.InstallInputs) render.InstallInputs {
			in.Netboot = &render.NetbootInputs{NFSRootURL: "h:/p"}
			return in
		}, "PXE is not supported"},
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
