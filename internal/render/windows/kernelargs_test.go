package windows

// boot.kernel_args / boot.installed_kernel_args are refused on windows
// (docs/04-install-spec.md §5.4): WinPE boots a BCD store, not a kernel
// command line — honest rejection at render, both pathways (setup.exe and
// the agent apply-image path share the gate).

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func TestKernelArgsRejected(t *testing.T) {
	d := New("windows2022")

	in := render.InstallInputs{
		TaskToken:       "tokW",
		MachineID:       "mch_w",
		Hostname:        "win-node",
		ImageSource:     "file:///data/win2022.iso",
		RootPassword:    "wRoot-pw",
		BootDrive:       "nvme0n1",
		AnswerBaseURL:   "https://m/render/tokW",
		CompleteURL:     "https://m/render/tokW/complete",
		AgentLogURL:     "https://m/render/tokW/agentlog",
		ExtraKernelArgs: "nomodeset",
		Disks: []render.ResolvedDisk{
			{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
				{Mount: "/boot/efi", FS: "vfat", SizeMB: 300, Flags: []string{"esp"}},
				{Mount: "/", FS: "ntfs", Grow: true},
			}},
		},
	}
	_, _, err := d.RenderAnswers(in, render.MachineView{})
	if err == nil || !strings.Contains(err.Error(), "kernel command line") {
		t.Fatalf("kernel_args not rejected: %v", err)
	}

	in.ExtraKernelArgs = ""
	in.InstalledKernelArgs = "modprobe.blacklist=nouveau"
	_, _, err = d.RenderAnswers(in, render.MachineView{})
	if err == nil || !strings.Contains(err.Error(), "installed_kernel_args") {
		t.Fatalf("installed_kernel_args not rejected: %v", err)
	}
}
