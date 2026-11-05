package provision

// boot.kernel_args / boot.installed_kernel_args parse gates (docs/04-install-spec.md
// §5.4): conservative charset + reserved keys. The canonical case the whole
// feature exists for — nouveau-hostile GPUs on unattended machines — must
// pass; everything that could break the iPXE/GRUB interpolation contexts or
// shadow engine-composed arguments must fail at submit, not on the machine.

import (
	"strings"
	"testing"
)

func TestValidateBootExtras(t *testing.T) {
	t.Parallel()
	ok := []string{
		"",                                     // absent
		"   ",                                  // whitespace only → no tokens
		"nomodeset",                            // the article's case, half one
		"nomodeset modprobe.blacklist=nouveau", // the article's case, full
		"nvme_core.io_timeout=240 pci=realloc",
		"iommu=pt amd_iommu=off",
		"libata.force=noncq",
		"modprobe.blacklist=nouveau rdblacklist=nouveau",
		"ipv6.disable=1",
	}
	for _, args := range ok {
		spec := &installSpecView{}
		spec.Boot.KernelArgs = args
		if err := validateBootExtras(spec); err != nil {
			t.Errorf("kernel_args %q rejected: %v", args, err)
		}
		spec = &installSpecView{}
		spec.Boot.InstalledKernelArgs = args
		if err := validateBootExtras(spec); err != nil {
			t.Errorf("installed_kernel_args %q rejected: %v", args, err)
		}
	}

	bad := []struct{ args, wantSubstr string }{
		{"quiet;reboot", "must match"},                // ; breaks GRUB (and smells like injection)
		{"console=ttyS0$()", "must match"},            // shell metacharacters
		{"rd.break=`id`", "must match"},               // backtick
		{"loglevel=\"7\"", "must match"},              // quoting
		{"ip=dhcp", "pipeline-owned"},                 // netboot plumbing
		{"BOOTIF=01-aa-bb", "pipeline-owned"},         // NIC binding
		{"console=ttyS0,115200n8", "pipeline-owned"},  // MAMMOTH_INSTALL_CONSOLE's
		{"inst.ks=http://x/ks.cfg", "pipeline-owned"}, // the seed carrier
		{"ds=nocloud-net;s=http://x", "must match"},   // ; anyway, and ds reserved
		{"modloop=http://x/modloop", "pipeline-owned"},
		{"modules=sd-mod,usb-storage", "pipeline-owned"},
	}
	for _, c := range bad {
		spec := &installSpecView{}
		spec.Boot.KernelArgs = c.args
		err := validateBootExtras(spec)
		if err == nil {
			t.Errorf("kernel_args %q accepted; want rejection naming %q", c.args, c.wantSubstr)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSubstr) {
			t.Errorf("kernel_args %q rejected with %v; want message naming %q", c.args, err, c.wantSubstr)
		}
	}
}
