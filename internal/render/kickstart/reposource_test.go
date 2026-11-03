package kickstart

// Carrier × source-shape matrix for the install source commands
// (docs/compat/distros.md PXE 安装源注记): every shape must render a source
// the INSTALLER can actually reach — wire findings 2288H 2026-11-03.

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func renderRepoCmd(t *testing.T, in render.InstallInputs) (string, string) {
	t.Helper()
	in.TaskToken = "tok"
	in.MachineID = "mch_x"
	in.BootDrive = "nvme0n1"
	in.Disks = []render.ResolvedDisk{{Device: "nvme0n1", Wipe: true, Partitions: []render.ResolvedPartition{
		{Mount: "/", FS: "xfs", Grow: true},
	}}}
	answers, boot, err := New("rocky9").RenderAnswers(in, render.MachineView{ID: in.MachineID, Hostname: in.Hostname})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ks := ""
	for _, a := range answers {
		if a.Name == "ks.cfg" {
			ks = a.Content
		}
	}
	for _, ln := range strings.Split(ks, "\n") {
		if strings.HasPrefix(ln, "url --url=") || strings.HasPrefix(ln, "nfs --server=") || ln == "cdrom" {
			return strings.TrimSpace(ln), boot.KernelArgs
		}
	}
	return "", boot.KernelArgs
}

func TestRepoCmdMatrix(t *testing.T) {
	base := render.InstallInputs{
		Hostname:      "node-01",
		RootPassword:  "s3creT-pw",
		SSHPublicKeys: []string{"ssh-ed25519 AAA k@h"},
		AnswerBaseURL: "https://m/render/tok",
		CompleteURL:   "https://m/render/tok/complete",
	}
	pooled := "http://m:8080/netboot/store/abc/iso"

	t.Run("pxe library reference: url points at the staged pool tree", func(t *testing.T) {
		in := base
		in.ImageSource = "/media/images/abc.iso" // resolved cache path
		in.Netboot = &render.NetbootInputs{PoolURL: pooled}
		cmd, args := renderRepoCmd(t, in)
		if cmd != "url --url="+pooled {
			t.Fatalf("repo cmd: %q", cmd)
		}
		if !strings.Contains(args, "inst.repo="+pooled) {
			t.Fatalf("inst.repo: %q", args)
		}
	})

	t.Run("pxe nfs source: nfs command wins over the pool url", func(t *testing.T) {
		in := base
		in.ImageSource = "nfs://192.168.1.132/data/media/images/x.iso"
		in.Netboot = &render.NetbootInputs{PoolURL: pooled}
		cmd, _ := renderRepoCmd(t, in)
		if cmd != "nfs --server=192.168.1.132 --dir=/data/media/images" {
			t.Fatalf("repo cmd: %q", cmd)
		}
	})

	t.Run("pxe http iso url: staged pool tree replaces the uninstallable iso url", func(t *testing.T) {
		in := base
		in.ImageSource = "https://mirror.example/Rocky-9.3.iso"
		in.Netboot = &render.NetbootInputs{PoolURL: pooled}
		cmd, _ := renderRepoCmd(t, in)
		if cmd != "url --url="+pooled {
			t.Fatalf("repo cmd: %q", cmd)
		}
	})

	t.Run("vmedia http tree url: url command kept (network packages)", func(t *testing.T) {
		in := base
		in.ImageSource = "https://mirror.example/rocky9"
		cmd, args := renderRepoCmd(t, in)
		if cmd != "url --url=https://mirror.example/rocky9" {
			t.Fatalf("repo cmd: %q", cmd)
		}
		if !strings.Contains(args, "inst.repo=https://mirror.example/rocky9") {
			t.Fatalf("inst.repo: %q", args)
		}
	})

	t.Run("vmedia library reference and iso urls: cdrom (packages on the mounted CD)", func(t *testing.T) {
		for _, src := range []string{"/media/images/abc.iso", "https://mirror.example/Rocky-9.3.iso", "file:///isos/x.iso"} {
			in := base
			in.ImageSource = src
			cmd, args := renderRepoCmd(t, in)
			if cmd != "cdrom" {
				t.Fatalf("src %q: repo cmd %q, want cdrom", src, cmd)
			}
			if !strings.Contains(args, "inst.repo=cdrom") {
				t.Fatalf("src %q: inst.repo %q, want cdrom", src, args)
			}
		}
	})

	t.Run("vmedia nfs source: nfs loop-mount of the hosted iso", func(t *testing.T) {
		in := base
		in.ImageSource = "nfs://192.168.1.132/data/media/images/x.iso"
		cmd, args := renderRepoCmd(t, in)
		if cmd != "nfs --server=192.168.1.132 --dir=/data/media/images" {
			t.Fatalf("repo cmd: %q", cmd)
		}
		if !strings.Contains(args, "inst.repo=nfs:192.168.1.132:/data/media/images/x.iso") {
			t.Fatalf("inst.repo: %q", args)
		}
	})
}
