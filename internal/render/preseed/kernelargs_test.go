package preseed

// boot.kernel_args / boot.installed_kernel_args rendering (docs/04-install-spec.md
// §5.4): both carriers' boot lines take the installer set; the post_install
// stage persists the installed set into GRUB_CMDLINE_LINUX ahead of the
// update-grub re-run.

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func TestKernelArgsBootLinesAndPersistence(t *testing.T) {
	d := New("debian12")
	in := render.InstallInputs{
		TaskToken:           "tokP",
		MachineID:           "mch_d",
		Hostname:            "gpu-node",
		ImageSource:         "https://mirror.example/debian-12.iso",
		RootPassword:        "dRoot-pw",
		BootDrive:           "nvme0n1",
		AnswerBaseURL:       "https://m/render/tokP",
		CompleteURL:         "https://m/render/tokP/complete",
		AgentLogURL:         "https://m/render/tokP/agentlog",
		ExtraKernelArgs:     "nomodeset modprobe.blacklist=nouveau",
		InstalledKernelArgs: "modprobe.blacklist=nouveau",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 512, Flags: []string{"esp"}},
				{Mount: "/", FS: "ext4", Grow: true},
			}},
		},
	}
	answers, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.HasSuffix(boot.KernelArgs, "nomodeset modprobe.blacklist=nouveau") {
		t.Errorf("ISO boot line missing declared args: %q", boot.KernelArgs)
	}

	byName := map[string]string{}
	for _, f := range answers {
		byName[f.Name] = f.Content
	}
	post, ok := byName["run/mammoth/post-install.sh"]
	if !ok {
		t.Fatalf("no post-install.sh in answers (%v)", byName)
	}
	// Persistence lands in /target's GRUB_CMDLINE_LINUX BEFORE the update-grub
	// re-run — that same regeneration picks the args up for free. The write
	// rides sed/echo double quotes, so the inner quotes appear
	// backslash-escaped in the script text.
	grubIdx := strings.Index(post, `GRUB_CMDLINE_LINUX=\"modprobe.blacklist=nouveau\"`)
	if grubIdx < 0 {
		t.Errorf("post-install.sh missing GRUB_CMDLINE_LINUX persistence:\n%s", post)
	}
	if ug := strings.Index(post, "in-target update-grub"); ug < 0 || (grubIdx >= 0 && ug < grubIdx) {
		t.Errorf("update-grub must run after the GRUB_CMDLINE_LINUX write (ug=%d grub=%d)", ug, grubIdx)
	}
}
