// Package debian implements the OSDriver for the debian-installer preseed
// dialect — Debian 12 and the UOS Server V20 derivative (a d-i fork) register
// as separate drivers of the same package. Mechanics: the preseed file is
// BAKED into the rebuilt boot ISO root and loaded offline via
// `file=/cdrom/preseed.cfg` (d-i mounts the boot medium at /cdrom) — the
// network preseed path needs installer early networking, which the ubuntu22
// real-hardware pass already proved unreliable. The questions d-i asks
// BEFORE the seed loads (locale, keyboard) ride as kernel arguments, which
// d-i treats as preseed values in their own right.
package debian

import (
	"fmt"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

// Driver is the debian-installer preseed driver. One instance per distro
// name ("debian12", "uniontechos") — the Registry routes by Distro() and one
// instance carries one name.
type Driver struct {
	distro string
}

// New returns the driver for one distro name.
func New(distro string) *Driver { return &Driver{distro: distro} }

func (d *Driver) Distro() string                { return d.distro }
func (d *Driver) SupportedArchs() []render.Arch { return []render.Arch{render.ArchAMD64} }

// KeepPartitionSupport: d-i can keep a whole disk (it never becomes a
// partman-auto target) but block-level partition reuse needs partman surgery
// — declared partial; keep: partitions is rejected at submit and at render
// (the same level as ubuntu22, docs/06-install-pipeline.md §5 matrix).
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportPartial }

// kernelArgs boots the d-i TEXT installer fully preseeded. auto=true turns on
// auto-install mode (priority=critical + automatic confirmation); file= loads
// the seed from the CD mount with zero networking; locale/keyboard precede
// the seed load, so they ride as kernel arguments — the early-question
// coverage that keeps the installer out of the language prompt.
const kernelArgs = "auto=true priority=critical file=/cdrom/preseed.cfg " +
	"debian-installer/locale=en_US.UTF-8 keyboard-configuration/layoutcode=us " +
	"console-setup/ask_detect=false console-setup/layoutcode=us"

// RenderAnswers produces the preseed file plus the pre/post install scripts,
// all baked into the rebuilt ISO root (SeedFiles). The scripts are separate
// files so every quote/JSON escape lives in shell, not in single-line preseed
// values (d-i preseed values cannot carry shell quoting reliably).
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if in.AnswerBaseURL == "" || in.CompleteURL == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: answer/completion URLs are required", d.distro)
	}
	if in.ImageSource == "" {
		return nil, render.BootParams{}, fmt.Errorf("%s: image source is required", d.distro)
	}

	target, err := installTarget(d.distro, in, m)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	recipe, err := expertRecipe(target)
	if err != nil {
		return nil, render.BootParams{}, err
	}
	net, err := netcfgSection(d.distro, in.Network, in.Hostname)
	if err != nil {
		return nil, render.BootParams{}, err
	}

	return []render.AnswerFile{
		{Name: "preseed.cfg", Content: d.preseed(in, target, recipe, net)},
		{Name: "run/mammoth/pre-install.sh", Content: preInstallScript(in)},
		{Name: "run/mammoth/post-install.sh", Content: postInstallScript(d.distro, in)},
	}, render.BootParams{
		AnswerURL:  strings.TrimSuffix(in.AnswerBaseURL, "/") + "/preseed.cfg",
		KernelArgs: kernelArgs,
	}, nil
}

// preseed assembles the answer file. The netinst ISO carries the base system,
// so the install source is the CD itself — mirror prompts are preseeded away
// and the install completes with zero network dependency.
func (d *Driver) preseed(in render.InstallInputs, t target, recipe, net string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Mammoth — task %s / machine %s\n", in.TaskToken, in.MachineID)
	b.WriteString("# Baked into the boot ISO root; loaded offline via file=/cdrom/preseed.cfg.\n")
	b.WriteString("# Locale/keyboard ALSO ride as kernel arguments: d-i asks them before\n")
	b.WriteString("# the seed loads (auto=true priority=critical suppresses everything else).\n\n")
	b.WriteString("#### locale / keyboard / clock\n")
	b.WriteString("d-i debian-installer/locale string en_US.UTF-8\n")
	b.WriteString("d-i keyboard-configuration/xkb-keymap select us\n")
	b.WriteString("d-i clock-setup/utc boolean true\n")
	b.WriteString("d-i time/zone string UTC\n")
	b.WriteString("d-i clock-setup/ntp boolean true\n")
	b.WriteString("d-i debian-installer/splash boolean false\n")
	b.WriteString("d-i hw-detect/load_firmware boolean true\n")
	b.WriteString("d-i debconf/frontend select text\n\n")
	b.WriteString("#### network (netcfg: one interface, static or dhcp)\n")
	b.WriteString(net)
	b.WriteString("\n#### install source: the netinst ISO carries the base system; no mirror\n")
	b.WriteString("d-i mirror/country string manual\n")
	b.WriteString("d-i apt-setup/use_mirror boolean false\n")
	b.WriteString("d-i apt-setup/services-select multiselect\n")
	b.WriteString("popularity-contest popularity-contest/participate boolean false\n\n")
	b.WriteString("#### account: root with the per-task password; no regular user\n")
	b.WriteString("d-i passwd/root-login boolean true\n")
	fmt.Fprintf(&b, "d-i passwd/root-password string %s\n", in.RootPassword)
	fmt.Fprintf(&b, "d-i passwd/root-password-again string %s\n", in.RootPassword)
	b.WriteString("d-i passwd/make-user boolean false\n")
	b.WriteString("d-i user-setup/allow-password-weak boolean true\n")
	b.WriteString("d-i user-setup/encrypt-home boolean false\n\n")
	b.WriteString("#### partitioning: expert recipe on the single install target\n")
	b.WriteString("d-i partman-auto/method string regular\n")
	b.WriteString("d-i partman-partitioning/default_label string gpt\n")
	fmt.Fprintf(&b, "d-i partman-auto/disk string /dev/%s\n", t.device)
	b.WriteString("d-i partman-auto/expert_recipe string \\\n")
	b.WriteString(recipe)
	b.WriteString("d-i partman-partitioning/confirm_write_new_label boolean true\n")
	b.WriteString("d-i partman/choose_partition select finish\n")
	b.WriteString("d-i partman/confirm boolean true\n")
	b.WriteString("d-i partman/confirm_nooverwrite boolean true\n")
	b.WriteString("d-i partman-md/confirm boolean true\n")
	b.WriteString("d-i partman-md/confirm_nooverwrite boolean true\n")
	b.WriteString("d-i partman-lvm/device_remove_lvm boolean true\n")
	b.WriteString("d-i partman-lvm/confirm boolean true\n")
	b.WriteString("d-i partman-lvm/confirm_nooverwrite boolean true\n")
	// Layouts without a declared swap partition: answer partman's "return to
	// the partitioning menu?" with No (false) instead of stalling, and skip
	// any swapfile prompt (real-hardware: the dialog appears even under
	// autoinstall's critical priority).
	b.WriteString("d-i partman-basicfilesystems/no_swap boolean false\n")
	b.WriteString("d-i partman-swapfile boolean false\n\n")
	b.WriteString("#### bootloader\n")
	b.WriteString("d-i grub-installer/only_debian boolean true\n")
	b.WriteString("d-i grub-installer/with_other_os boolean false\n")
	fmt.Fprintf(&b, "d-i grub-installer/bootdev string /dev/%s\n\n", t.device)
	b.WriteString("#### packages: standard system + ssh server, nothing else\n")
	b.WriteString("tasksel tasksel/first multiselect standard\n")
	b.WriteString("d-i pkgsel/include string openssh-server\n")
	b.WriteString("d-i pkgsel/update-policy select none\n")
	b.WriteString("d-i pkgsel/upgrade select none\n\n")
	b.WriteString("#### finish: hooks live on the boot medium (see run/mammoth/*.sh)\n")
	b.WriteString("d-i debian-installer/exit/poweroff boolean false\n")
	b.WriteString("d-i preseed/early_command string sh /cdrom/run/mammoth/pre-install.sh\n")
	b.WriteString("d-i preseed/late_command string sh /cdrom/run/mammoth/post-install.sh\n")
	return b.String()
}
