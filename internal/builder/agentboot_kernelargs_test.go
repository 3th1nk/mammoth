package builder

// The agent runtime's grub.cfg write expands MAMMOTH_INSTALLED_KERNEL_ARGS
// (boot.installed_kernel_args, docs/04-install-spec.md §5.4): the heredoc's
// ${var:+ …} param-expansion appends the plan-declared tokens after `quiet`
// and no-ops to the bare line when the declaration is absent.

import (
	"strings"
	"testing"
)

func TestRuntimeGrubLineExpandsInstalledArgs(t *testing.T) {
	script := agentScript()
	line := "linux /boot/vmlinuz-$kflavor root=UUID=$rootuuid modules=sd-mod,usb-storage,ext4 quiet${MAMMOTH_INSTALLED_KERNEL_ARGS:+ $MAMMOTH_INSTALLED_KERNEL_ARGS}"
	if !strings.Contains(script, line) {
		t.Errorf("runtime grub.cfg line does not expand MAMMOTH_INSTALLED_KERNEL_ARGS")
	}
}
