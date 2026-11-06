// Package dd implements the OSDriver for image-direct-write installs
// (roadmap「镜像直写通路」, docs/09): the machine boots the agent initramfs
// (docs/12, third payload — same alpine netboot tarball + apkovl overlay as
// the alpine pilot and the windows apply-image path) and streams a
// pre-built whole-disk image onto the target device (wget | gunzip | tee |
// sha256sum — single pass, checksum verified on the bytes that hit the
// disk). The image carries its own partition table/ESP and the target OS;
// the engine contributes intent-free transport here — firstboot intent
// (identity/network/access) rides a NoCloud seed in slice 2 and is honestly
// rejected until then (docs/security-baseline posture: gates at render,
// never silent).
package dd

import (
	"fmt"
	"strings"

	"github.com/3th1nk/mammoth/internal/render"
)

type Driver struct {
	distro string
}

func New(distro string) *Driver { return &Driver{distro: distro} }

func (d *Driver) Distro() string { return d.distro }
func (d *Driver) SupportedArchs() []render.Arch {
	return []render.Arch{render.ArchAMD64, render.ArchARM64}
}

// Family: its own family — the verify_ready short-circuit keys on it (the
// completion report from the agent env IS the verification surface: the
// image's first boot is the image's own business, not a mammoth pipeline).
func (d *Driver) Family() string { return "dd" }

// KeepPartitionSupport: the target disk is consumed whole by the image —
// there is nothing to keep.
func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportNone }

// PXESupport: full — same alpine netboot carrier as the agent pilot
// (tarball + modloop + apkovl over HTTP, busybox-only runtime: no apks).
func (d *Driver) PXESupport() render.SupportLevel { return render.SupportFull }

func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierAlpineNetboot }

// NetbootPool: none — the runtime is busybox-only (dd/wget/gunzip/tee/
// sha256sum are all built-ins); no apks subtree in the boot tree.
func (d *Driver) NetbootPool() render.NetbootPool { return render.NetbootPoolNone }

// AgentInstaller: the boot carrier must add the agent apkovl overlay to the
// boot media.
func (d *Driver) AgentInstaller() bool { return true }

// planName is the runtime's scan target (find_plan in the shared agent
// runtime script looks for agent-plan.sh — one overlay, every payload).
const planName = "agent-plan.sh"

// RenderAnswers produces the dd plan (sh) and the boot parameters. The plan
// is pure data declarations — the runtime's dd branch consumes them.
func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if err := validate(in); err != nil {
		return nil, render.BootParams{}, fmt.Errorf("%s: %w", d.distro, err)
	}
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")

	sh := renderSH(in)
	return []render.AnswerFile{
			{Name: planName, Content: sh},
		}, render.BootParams{
			AnswerURL:  base + "/" + planName,
			KernelArgs: render.WithExtraArgs(isoKernelArgs, in.ExtraKernelArgs),
			// PXE-only (slice 1): the vMedia carrier would need an agent ISO
			// assembly — NetbootKernelArgs carries the boot on the netboot
			// tree alone.
			NetbootKernelArgs: render.WithExtraArgs(netbootKernelArgs(in), in.ExtraKernelArgs),
		}, nil
}

// isoKernelArgs boots the agent from a repacked ISO (unused on the PXE-only
// pathway today; kept in the same shape as the agent driver's).
const isoKernelArgs = "modules=loop,squashfs,sd-mod,usb-storage,ext4,vfat console=tty0 console=ttyS0,115200"

// netbootKernelArgs boots the agent over PXE: early DHCP, the network
// modloop, the apkovl overlay and the plan base — the agent carrier's URL
// derivation, minus alpine_repo (busybox-only runtime: no apks subtree).
func netbootKernelArgs(in render.InstallInputs) string {
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")
	ext := strings.TrimSuffix(base, "/render/"+in.TaskToken)
	files := ext + "/netboot/files/" + in.TaskToken
	args := "modules=loop,squashfs,sd-mod,usb-storage console=tty0 console=ttyS0,115200 ip=dhcp"
	args += " modloop=" + files + "/modloop"
	args += " apkovl=" + files + "/agent.apkovl.tar.gz"
	args += " mammoth_base=" + base
	return args
}

// validate rejects what the dd pathway cannot express at the transport
// level (input-visible only — spec-level intent gates live in provision's
// validateDD where the raw spec is at hand): the whole-disk image owns the
// target, and the stream verification needs the declared checksum.
func validate(in render.InstallInputs) error {
	if in.ImageSource == "" {
		return fmt.Errorf("image.source is required (http(s) URL of the whole-disk image)")
	}
	if !strings.HasPrefix(in.ImageSource, "http://") && !strings.HasPrefix(in.ImageSource, "https://") {
		return fmt.Errorf("image.source must be http(s) — the machine streams the image directly (file:// exists only on the engine host)")
	}
	if in.ImageChecksum == "" {
		return fmt.Errorf("image.checksum is required — the write is verified by sha256 of the stream that hit the disk (uncompressed-image hash)")
	}
	if len(in.Disks) != 1 {
		return fmt.Errorf("dd takes exactly one target disk selector; got %d", len(in.Disks))
	}
	d := in.Disks[0]
	if len(d.Partitions) > 0 {
		return fmt.Errorf("dd writes the whole disk — the image carries its own partition table; declare no partitions on the target")
	}
	if in.Netboot == nil {
		return fmt.Errorf("dd installs are PXE-only (boot.strategy=pxe) — the alpine netboot carrier has no vMedia assembly")
	}
	return nil
}

// renderSH emits the data declarations the agent runtime sources: one line
// per fact, quote-safe (the agent plan's executable face).
func renderSH(in render.InstallInputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth dd plan — task %s / machine %s\n", in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "MAMMOTH_DD_MODE=1\n")
	fmt.Fprintf(&b, "MAMMOTH_DD_URL=%s\n", shQuote(in.ImageSource))
	fmt.Fprintf(&b, "MAMMOTH_DD_SHA256=%s\n", shQuote(strings.ToLower(in.ImageChecksum)))
	fmt.Fprintf(&b, "MAMMOTH_DD_FORMAT=%s\n", shQuote(in.ImageFormat))
	fmt.Fprintf(&b, "MAMMOTH_DD_TARGET=%s\n", shQuote(in.Disks[0].Device))
	fmt.Fprintf(&b, "MAMMOTH_COMPLETE_URL=%s\n", shQuote(in.CompleteURL))
	fmt.Fprintf(&b, "MAMMOTH_LOG_URL=%s\n", shQuote(in.AgentLogURL))
	return b.String()
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
