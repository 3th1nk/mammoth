package kickstart

// boot.kernel_args / boot.installed_kernel_args rendering (docs/04-install-spec.md
// §5.4): the installer-environment set lands last on the boot line, the
// installed-system set rides bootloader --append (both the static template
// line and the %pre-generated storage include).

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func kernelArgsInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken:           "tok123",
		MachineID:           "mch_x",
		Hostname:            "gpu-node",
		ImageSource:         "https://mirror.example/rocky9",
		RootPassword:        "s3creT-pw",
		BootDrive:           "nvme0n1",
		AnswerBaseURL:       "https://m/render/tok123",
		CompleteURL:         "https://m/render/tok123/complete",
		ExtraKernelArgs:     "nomodeset modprobe.blacklist=nouveau",
		InstalledKernelArgs: "modprobe.blacklist=nouveau",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "xfs", Grow: true},
			}},
		},
	}
}

func TestKernelArgsBootLineAndAppend(t *testing.T) {
	d := New("rocky9")
	answers, boot, err := d.RenderAnswers(kernelArgsInputs(), render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content

	// Installer boot line: extra tokens ride at the end (after inst.ks/inst.repo,
	// so last-wins parameters resolve the user's way).
	if !strings.HasSuffix(boot.KernelArgs, "nomodeset modprobe.blacklist=nouveau") {
		t.Errorf("boot line does not end with the declared args: %q", boot.KernelArgs)
	}
	if strings.Contains(boot.KernelArgs, "modprobe.blacklist=nouveau nomodeset modprobe.blacklist=nouveau") {
		t.Errorf("declared args duplicated on the boot line: %q", boot.KernelArgs)
	}

	// Installed system: bootloader --append carries the persistent set — on
	// the static template line (boot drive known at render).
	if !strings.Contains(ks, `--append="modprobe.blacklist=nouveau"`) {
		t.Errorf("ks.cfg missing bootloader --append:\n%s", ks)
	}
}

func TestKernelArgsAppendInStorageInclude(t *testing.T) {
	// Controller-named disk (non-kernel name) moves the bootloader line into
	// the %pre-generated include — --append must ride along there too.
	in := kernelArgsInputs()
	in.Disks[0].Device = "LogicalDrive0"
	d := New("rocky9")
	answers, _, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := answers[0].Content
	if !strings.Contains(ks, "90-storage.ks") {
		t.Fatalf("no storage include rendered (test premise broken)")
	}
	if !strings.Contains(ks, `--append="modprobe.blacklist=nouveau"`) {
		t.Errorf("storage include lost bootloader --append:\n%s", ks)
	}
}

func TestNoKernelArgsNoAppend(t *testing.T) {
	in := kernelArgsInputs()
	in.ExtraKernelArgs = ""
	in.InstalledKernelArgs = ""
	d := New("rocky9")
	answers, boot, err := d.RenderAnswers(in, render.MachineView{ID: "mch_x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(boot.KernelArgs, "nomodeset") || strings.Contains(answers[0].Content, "--append") {
		t.Errorf("absent declaration leaked into the render")
	}
}
