package autoinstall

// boot.kernel_args / boot.installed_kernel_args rendering (docs/04-install-spec.md
// §5.4): both carriers' boot lines take the installer set; late-commands
// persist the installed set into GRUB_CMDLINE_LINUX and re-run update-grub.

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func kernelArgsInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken:     "tok9",
		MachineID:     "mch_u",
		Hostname:      "gpu-node",
		ImageSource:   "https://mirror.example/ubuntu-22.04.iso",
		RootPassword:  "uRoot-pw",
		BootDrive:     "nvme0n1",
		AnswerBaseURL: "https://m/render/tok9",
		CompleteURL:   "https://m/render/tok9/complete",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
			}},
		},
	}
}

func TestKernelArgsBothCarriersAndPersistence(t *testing.T) {
	d := New("ubuntu22")
	in := kernelArgsInputs()
	in.ExtraKernelArgs = "nomodeset modprobe.blacklist=nouveau"
	in.InstalledKernelArgs = "modprobe.blacklist=nouveau"
	answers, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Installer environment: the ISO line carries the set.
	if !strings.HasSuffix(boot.KernelArgs, "nomodeset modprobe.blacklist=nouveau") {
		t.Errorf("ISO boot line missing declared args: %q", boot.KernelArgs)
	}

	// Installed system: GRUB_CMDLINE_LINUX (not _DEFAULT — recovery entries
	// included) + the update-grub regeneration. The late-command rides the
	// JSON user-data, so the inner quotes appear JSON-escaped.
	_, ud := fetchUser(t, answers)
	late := lateCommands(t, ud)
	joined := strings.Join(late, "\n")
	// The value rides sed's double-quoted replacement (and the echo
	// fallback), so the inner quotes appear backslash-escaped in the shell
	// text — assert that form.
	if !strings.Contains(joined, `GRUB_CMDLINE_LINUX=\"modprobe.blacklist=nouveau\"`) {
		t.Errorf("late-commands missing GRUB_CMDLINE_LINUX persistence:\n%s", joined)
	}
	if !strings.Contains(joined, "update-grub") {
		t.Errorf("late-commands missing the update-grub regeneration:\n%s", joined)
	}
}

func lateCommands(t *testing.T, ud map[string]any) []string {
	t.Helper()
	auto, ok := ud["autoinstall"].(map[string]any)
	if !ok {
		t.Fatalf("user-data has no autoinstall block")
	}
	raw, ok := auto["late-commands"].([]any)
	if !ok {
		t.Fatalf("autoinstall has no late-commands list")
	}
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.(string))
	}
	return out
}

func TestKernelArgsNetbootLine(t *testing.T) {
	d := New("ubuntu22")
	in := kernelArgsInputs()
	in.Netboot = &render.NetbootInputs{NFSRootURL: "10.0.0.1:/srv/pool/store/abc/iso"}
	in.ExtraKernelArgs = "modprobe.blacklist=nouveau"
	_, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.HasSuffix(boot.NetbootKernelArgs, "modprobe.blacklist=nouveau") {
		t.Errorf("netboot line missing declared args (after BOOTIF): %q", boot.NetbootKernelArgs)
	}
}
