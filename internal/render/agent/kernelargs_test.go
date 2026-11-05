package agent

// boot.kernel_args / boot.installed_kernel_args on the agent carrier
// (docs/04-install-spec.md §5.4): both boot lines take the installer set;
// the plan carries MAMMOTH_INSTALLED_KERNEL_ARGS for the runtime's grub.cfg
// write (the runtime side expands it — asserted in internal/builder).

import (
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/internal/render"
)

func TestKernelArgsBootLinesAndPlan(t *testing.T) {
	d := New("alpine")
	in := baseInputs()
	in.ExtraKernelArgs = "nomodeset modprobe.blacklist=nouveau"
	in.InstalledKernelArgs = "modprobe.blacklist=nouveau"
	files, boot, err := d.RenderAnswers(in, render.MachineView{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.HasSuffix(boot.KernelArgs, "nomodeset modprobe.blacklist=nouveau") {
		t.Errorf("ISO boot line missing declared args: %q", boot.KernelArgs)
	}

	byName := map[string]string{}
	for _, f := range files {
		byName[f.Name] = f.Content
	}
	sh, ok := byName[answerSH]
	if !ok {
		t.Fatalf("no %s in answers", answerSH)
	}
	if !strings.Contains(sh, `MAMMOTH_INSTALLED_KERNEL_ARGS='modprobe.blacklist=nouveau'`) {
		t.Errorf("plan missing MAMMOTH_INSTALLED_KERNEL_ARGS:\n%s", sh)
	}
}
