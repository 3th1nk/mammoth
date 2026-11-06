package dd

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func ddInputs() render.InstallInputs {
	return render.InstallInputs{
		TaskToken:     "tokDD",
		MachineID:     "mch_dd",
		ImageSource:   "http://10.0.0.9/images/thor.img.gz",
		ImageChecksum: "ABCDEF0123456789",
		ImageFormat:   "raw.gz",
		AnswerBaseURL: "http://10.0.0.1:8080/render/tokDD",
		CompleteURL:   "http://10.0.0.1:8080/render/tokDD/complete",
		AgentLogURL:   "http://10.0.0.1:8080/render/tokDD/agentlog",
		Netboot:       &render.NetbootInputs{},
		Disks:         []render.ResolvedDisk{{Device: "nvme0n1", Wipe: true}},
	}
}

func TestRenderDDPlan(t *testing.T) {
	d := New("dd")
	answers, boot, err := d.RenderAnswers(ddInputs(), render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(answers) != 1 || answers[0].Name != "agent-plan.sh" {
		t.Fatalf("want single agent-plan.sh, got %+v", answers)
	}
	sh := answers[0].Content
	for _, want := range []string{
		"MAMMOTH_DD_MODE=1",
		`MAMMOTH_DD_URL='http://10.0.0.9/images/thor.img.gz'`,
		`MAMMOTH_DD_SHA256='abcdef0123456789'`, // normalized lowercase
		`MAMMOTH_DD_FORMAT='raw.gz'`,
		`MAMMOTH_DD_TARGET='nvme0n1'`,
		`MAMMOTH_COMPLETE_URL='http://10.0.0.1:8080/render/tokDD/complete'`,
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("plan missing %q:\n%s", want, sh)
		}
	}
	// Boot line: agent carrier URLs, NO alpine_repo (busybox-only runtime).
	nb := boot.NetbootKernelArgs
	for _, want := range []string{"modloop=", "apkovl=", "mammoth_base=", "ip=dhcp"} {
		if !strings.Contains(nb, want) {
			t.Errorf("netboot args missing %q: %s", want, nb)
		}
	}
	if strings.Contains(nb, "alpine_repo") {
		t.Errorf("dd boot line carries alpine_repo (no apks in the tree): %s", nb)
	}
}

func TestRenderDDRejects(t *testing.T) {
	d := New("dd")
	cases := []struct {
		name string
		mut  func(render.InstallInputs) render.InstallInputs
		want string
	}{
		{"no-checksum", func(i render.InstallInputs) render.InstallInputs { i.ImageChecksum = ""; return i }, "checksum is required"},
		{"file-source", func(i render.InstallInputs) render.InstallInputs { i.ImageSource = "file:///data/x.img"; return i }, "must be http(s)"},
		{"partitions", func(i render.InstallInputs) render.InstallInputs {
			i.Disks[0].Partitions = []render.ResolvedPartition{{Mount: "/", FS: "ext4", Grow: true}}
			return i
		}, "whole disk"},
		{"two-disks", func(i render.InstallInputs) render.InstallInputs {
			i.Disks = append(i.Disks, render.ResolvedDisk{Device: "nvme1n1", Wipe: true})
			return i
		}, "exactly one"},
		{"vmedia", func(i render.InstallInputs) render.InstallInputs { i.Netboot = nil; return i }, "PXE-only"},
	}
	for _, c := range cases {
		_, _, err := d.RenderAnswers(c.mut(ddInputs()), render.MachineView{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}
