// Package preseed implements the OSDriver for the debian-installer preseed
// dialect (Debian 12 — docs/06-install-pipeline.md §5). UOS Server V20 was
// moved to the kickstart package: ISO inspection showed its installer is an
// anaconda derivative with an RHEL-style tree, not a d-i fork. Mechanics: the preseed file is
// BAKED into the rebuilt boot ISO root and loaded offline via
// `file=/cdrom/preseed.cfg` (d-i mounts the boot medium at /cdrom) — the
// network preseed path needs installer early networking, which the ubuntu22
// real-hardware pass already proved unreliable. The questions d-i asks
// BEFORE the seed loads (locale, keyboard) ride as kernel arguments, which
// d-i treats as preseed values in their own right.
package preseed

import (
	"fmt"
	"net/url"
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

// PXESupport: full over the netboot tarball (MAMMOTH_PXE_DEBIAN12_NETBOOT)
// with the install source served as an HTTP pool unpacked from the distro
// ISO — offline semantics kept, no upstream mirror (docs/06-install-pipeline.md
// §3.3, §6).
func (d *Driver) PXESupport() render.SupportLevel { return render.SupportFull }

// NetbootCarrier: the ISO's own d-i initrd is the cdrom flavour — useless
// over the wire; the boot files come from the distro's official netboot
// tarball instead.
func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierDINetboot }

// NetbootPool: the ISO unpacks under the boot tree and serves as d-i's
// HTTP mirror (dists/ + pool/), keyed by suite.
func (d *Driver) NetbootPool() render.NetbootPool { return render.NetbootPoolHTTP }

// suite is the archive codename the pool's dists/ carries — choose-mirror
// needs it preseeded because the pool layout has no Release label prompt.
func (d *Driver) suite() (string, error) {
	switch d.distro {
	case "debian12":
		return "bookworm", nil
	default:
		return "", fmt.Errorf("%s: no archive suite mapped for PXE installs", d.distro)
	}
}

// kernelArgs boots the d-i TEXT installer fully preseeded. auto=true turns on
// auto-install mode (priority=critical + automatic confirmation); file= loads
// the seed from the CD mount with zero networking; locale/keyboard precede
// the seed load, so they ride as kernel arguments — the early-question
// coverage that keeps the installer out of the language prompt.
const kernelArgs = "auto=true priority=critical file=/cdrom/preseed.cfg " +
	"debian-installer/locale=en_US.UTF-8 keyboard-configuration/layoutcode=us " +
	"console-setup/ask_detect=false console-setup/layoutcode=us"

// netbootKernelArgs swaps the seed carrier: preseed/url fetches over HTTP —
// the netboot initrd has no CD to mount. The early-question kernel arguments
// are identical; the seed body differs only in its install-source section.
func netbootKernelArgs(answerBaseURL, seedName string) string {
	return "auto=true priority=critical preseed/url=" +
		strings.TrimSuffix(answerBaseURL, "/") + "/" + seedName + " " +
		"debian-installer/locale=en_US.UTF-8 keyboard-configuration/layoutcode=us " +
		"console-setup/ask_detect=false console-setup/layoutcode=us"
}

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

	// Two seed carriers, one body: the netboot variant differs only in its
	// install-source section (HTTP pool mirror instead of the CD mount).
	seedName, bootArgs := "preseed.cfg", kernelArgs
	if in.Netboot != nil {
		seedName = "preseed-netboot.cfg"
		bootArgs = netbootKernelArgs(in.AnswerBaseURL, seedName)
	}

	return []render.AnswerFile{
			{Name: seedName, Content: d.preseed(in, target, recipe, net, in.Netboot)},
			{Name: "run/mammoth/pre-install.sh", Content: preInstallScript(in)},
			{Name: "run/mammoth/post-install.sh", Content: postInstallScript(d.distro, in)},
		}, render.BootParams{
			AnswerURL:         strings.TrimSuffix(in.AnswerBaseURL, "/") + "/" + seedName,
			KernelArgs:        kernelArgs,
			NetbootKernelArgs: bootArgs,
		}, nil
}

// preseed assembles the answer file. Offline (nil netboot) the netinst ISO
// carries the base system — the install source is the CD itself and mirror
// prompts are preseeded away. Over PXE the source is the HTTP pool unpacked
// from the same ISO under the boot tree: the mirror points there (dists/ +
// pool/), keeping the offline semantics with no upstream mirror.
func (d *Driver) preseed(in render.InstallInputs, t target, recipe, net string, nb *render.NetbootInputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Mammoth — task %s / machine %s\n", in.TaskToken, in.MachineID)
	if nb == nil {
		b.WriteString("# Baked into the boot ISO root; loaded offline via file=/cdrom/preseed.cfg.\n")
	} else {
		b.WriteString("# Served over HTTP for the PXE path; loaded via preseed/url (netboot initrd\n")
		b.WriteString("# has no CD to mount). Install source: the distro ISO unpacked under the\n")
		b.WriteString("# task boot tree, consumed as a plain HTTP pool.\n")
	}
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
	if nb == nil {
		b.WriteString("\n#### install source: the netinst ISO carries the base system; no mirror\n")
		b.WriteString("d-i mirror/country string manual\n")
		b.WriteString("d-i apt-setup/use_mirror boolean false\n")
		b.WriteString("d-i apt-setup/services-select multiselect\n")
		b.WriteString("popularity-contest popularity-contest/participate boolean false\n")
		// Single-medium installs must not let apt hunt for other discs: the
		// standard taskset is NOT fully present in the netinst pool (it expects
		// a mirror), which otherwise loops apt on "Please insert the media
		// labeled ..." forever (real-hardware). Base + pkgsel/include covers the
		// provisioning contract; extra packages ride the config channel.
		b.WriteString("d-i apt-setup/cdrom/set-first boolean false\n")
		b.WriteString("d-i apt-setup/cdrom/set-next boolean false\n")
		b.WriteString("d-i apt-setup/cdrom/set-double boolean false\n\n")
	} else {
		suite, err := d.suite()
		if err != nil {
			// Rendered seeds are already validated upstream (suite mapping is
			// driver-static); this guard keeps the template total.
			suite = "stable"
		}
		u, perr := url.Parse(nb.PoolURL)
		host, dir := u.Host, u.Path
		if perr != nil || host == "" {
			host, dir = nb.PoolURL, ""
		}
		fmt.Fprintf(&b, "\n#### install source: the distro ISO unpacked under the boot tree, over HTTP\n")
		b.WriteString("d-i mirror/country string manual\n")
		// choose-mirror accepts host:port in the hostname — mammoth's machine
		// face is rarely on port 80 (qemu verification pending).
		fmt.Fprintf(&b, "d-i mirror/http/hostname string %s\n", host)
		fmt.Fprintf(&b, "d-i mirror/http/directory string %s\n", dir)
		fmt.Fprintf(&b, "d-i mirror/suite string %s\n", suite)
		b.WriteString("d-i apt-setup/use_mirror boolean false\n")
		b.WriteString("d-i apt-setup/services-select multiselect\n")
		b.WriteString("popularity-contest popularity-contest/participate boolean false\n\n")
	}
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
	// No tasksel taskset: the standard task's packages are not fully in the
	// netinst pool — offline installs would loop apt on a media-change
	// prompt forever (real-hardware). Base + pkgsel/include carries the
	// provisioning contract; extra packages ride the config channel.
	b.WriteString("#### packages: base + ssh server only\n")
	b.WriteString("tasksel tasksel/first multiselect\n")
	b.WriteString("d-i pkgsel/include string openssh-server\n")
	b.WriteString("d-i pkgsel/update-policy select none\n")
	b.WriteString("d-i pkgsel/upgrade select none\n\n")
	b.WriteString("#### finish: hooks live on the boot medium (see run/mammoth/*.sh)\n")
	b.WriteString("d-i debian-installer/exit/poweroff boolean false\n")
	b.WriteString("d-i preseed/early_command string sh /cdrom/run/mammoth/pre-install.sh\n")
	b.WriteString("d-i preseed/late_command string sh /cdrom/run/mammoth/post-install.sh\n")
	return b.String()
}
