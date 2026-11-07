// Package bootc renders the bootc/OCI install pathway (docs/09-roadmap.md
// 下一阶段 16): the spec's distro key IS the pathway switch — the agent
// runtime pulls the declared OCI image and runs `bootc install to-disk`
// inside a container of that image (bootc's documented install shape: the
// command must run from the image being installed). bootc owns the disk
// layout (DPS GUIDs: root + ESP + BIOS boot); the engine owns intent
// delivery — ssh keys ride bootc's native --root-ssh-authorized-keys.
// SLICE-1 BOUNDARY (render-time honest reject): identity/network/password
// intent is not carried yet — bootc's layout has no CIDATA slot (the dd
// seed contract needs one) and the sfdisk append is dead on MegaRAID
// (2288H, ab-v22..26); the later CIDATA-via-mount slice reopens it
// (bootc install mount → /var/lib/cloud/seed/nocloud, image cloud-init
// required).
package bootc

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

func (d *Driver) Family() string { return "bootc" }

func (d *Driver) KeepPartitionSupport() render.SupportLevel { return render.SupportNone }

func (d *Driver) PXESupport() render.SupportLevel { return render.SupportFull }

// NetbootCarrier mirrors dd: the agent RAM OS is the runtime, the alpine
// NETBOOT tree is the PXE carrier (NIC drivers live there).
func (d *Driver) NetbootCarrier() render.NetbootCarrier { return render.NetbootCarrierAlpineNetboot }

// NetbootPoolNone: no distro tree to unpack — the payload is podman + the
// pulled OCI image, and the tool pool (extended ISO /apks) carries podman.
func (d *Driver) NetbootPool() render.NetbootPool { return render.NetbootPoolNone }

func (d *Driver) AgentInstaller() bool { return true }

const planName = "agent-plan.sh"

// isoKernelArgs boots the agent from a repacked ISO (the dd line's shape).
const isoKernelArgs = "modules=loop,squashfs,sd-mod,usb-storage,ext4,vfat console=ttyS0,115200 console=tty0"

// netbootKernelArgs boots the agent over PXE: early DHCP, the network
// modloop, the apkovl overlay and the plan base — the agent carrier's URL
// derivation. alpine_repo rides unconditionally: podman comes from the
// extended pool (the pathway's hard prerequisite).
func netbootKernelArgs(in render.InstallInputs) string {
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")
	ext := strings.TrimSuffix(base, "/render/"+in.TaskToken)
	files := ext + "/netboot/files/" + in.TaskToken
	args := "modules=loop,squashfs,sd-mod,usb-storage console=ttyS0,115200 console=tty0 ip=dhcp"
	args += " modloop=" + files + "/modloop"
	args += " apkovl=" + files + "/agent.apkovl.tar.gz"
	args += " mammoth_base=" + base
	args += " alpine_repo=" + files + "/apks"
	return args
}

func (d *Driver) RenderAnswers(in render.InstallInputs, m render.MachineView) ([]render.AnswerFile, render.BootParams, error) {
	if err := validate(in); err != nil {
		return nil, render.BootParams{}, fmt.Errorf("%s: %w", d.distro, err)
	}
	base := strings.TrimSuffix(in.AnswerBaseURL, "/")

	sh := renderSH(in)
	// Both carriers stage the extended pool (podman's apk source): on PXE
	// it rides the netboot tree, on vMedia the virtualmedia strategy stages
	// the same subtree under the task's netboot path (the ISO boot line
	// points there). The agent plan and the apkovl overlay ride the ISO
	// root on vMedia (find_plan scans the media root).
	isoArgs := isoKernelArgs + " alpine_repo=" + strings.TrimSuffix(base, "/render/"+in.TaskToken) + "/netboot/files/" + in.TaskToken + "/apks"
	return []render.AnswerFile{
			{Name: planName, Content: sh},
		}, render.BootParams{
			AnswerURL: base + "/" + planName,
			// The runtime reports applied and reboots by itself (the image's
			// first boot needs no engine nudge) — the completion tail must
			// not cycle a machine that is already booting its new image.
			InstallerAutoReboot: true,
			KernelArgs:          render.WithExtraArgs(isoArgs, in.ExtraKernelArgs),
			NetbootKernelArgs:   render.WithExtraArgs(netbootKernelArgs(in), in.ExtraKernelArgs),
		}, nil
}

// validate rejects what the bootc pathway cannot express (input-visible
// only — spec-level intent gates live in provision's validateBootc where
// the raw spec is at hand): the image ref must be an OCI reference, and the
// seed surface gates (bond/vlan/MAC-match) are the shared nocloud's.
func validate(in render.InstallInputs) error {
	if in.ImageSource == "" {
		return fmt.Errorf("image.source is required (OCI reference: registry/repo:tag, digest-pinned registry/repo@sha256:... recommended)")
	}
	if _, err := imageRef(in.ImageSource); err != nil {
		return err
	}
	// Slice-1 intent boundary: keys ride the native bootc flag (NOT the
	// seed — nocloud.SeedIntent counts them, dd's CIDATA semantics); every
	// other seed-carried intent is an honest reject (see package doc) —
	// never silently dropped intent.
	if in.Hostname != "" || in.RootPassword != "" || len(in.Network) > 0 {
		return fmt.Errorf("bootc slice 1 carries access.ssh_keys only — hostname/network/root_password intent needs the image's own cloud-init/ignition config (the CIDATA-via-mount slice reopens this)")
	}
	return nil
}

// imageRef normalizes the declared OCI reference: a bare
// registry/repo[:tag][@digest] passes through; an explicit docker:// scheme
// is stripped (containers-transports form). Everything else — http(s) ISO
// URLs, file/nfs paths — is a schema error: the pull is podman's job, the
// transport is not an ISO fetch.
func imageRef(source string) (string, error) {
	ref := strings.TrimPrefix(source, "docker://")
	if ref == "" {
		return "", fmt.Errorf("image.source: empty OCI reference")
	}
	lower := strings.ToLower(ref)
	for _, bad := range []string{"http://", "https://", "file://", "nfs://", "/"} {
		if strings.HasPrefix(lower, bad) {
			return "", fmt.Errorf("image.source %q is not an OCI reference — bootc installs from a registry (registry/repo:tag, digest-pinned @sha256:... recommended)", source)
		}
	}
	if !strings.Contains(ref, ":") && !strings.Contains(ref, "@") {
		// bare registry/repo — podman defaults :latest; keep the operator's
		// spelling but the digest-pinned form is the documented recommendation
		return ref, nil
	}
	return ref, nil
}

// renderSH emits the data declarations the agent runtime sources (the dd
// plan's executable-face shape): the dispatch flag, the OCI reference, the
// resolved target device, and the token-scoped report/log URLs.
func renderSH(in render.InstallInputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth bootc plan — task %s / machine %s\n", in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "MAMMOTH_BOOTC=1\n")
	fmt.Fprintf(&b, "MAMMOTH_BOOTC_REF=%s\n", shQuote(mustImageRef(in.ImageSource)))
	// The kernel device name is a machine-side fact (controller names exist
	// only in the inventory — the dd resolver idiom); the runtime resolves
	// by size when the inventory name has no kernel node.
	fmt.Fprintf(&b, "MAMMOTH_BOOTC_TARGET=%s\n", shQuote(in.Disks[0].Device))
	fmt.Fprintf(&b, "MAMMOTH_BOOTC_SIZE_BYTES=%s\n", fmt.Sprintf("%d", in.Disks[0].SizeBytes))
	// ssh keys ride bootc's native --root-ssh-authorized-keys (a tmpfiles
	// config that re-applies them every boot — stronger than a one-shot
	// authorized_keys write).
	fmt.Fprintf(&b, "MAMMOTH_SSH_KEYS=%s\n", shQuote(strings.Join(in.SSHPublicKeys, "\n")))
	fmt.Fprintf(&b, "MAMMOTH_COMPLETE_URL=%s\n", shQuote(in.CompleteURL))
	fmt.Fprintf(&b, "MAMMOTH_LOG_URL=%s\n", shQuote(in.AgentLogURL))
	return b.String()
}

func mustImageRef(source string) string {
	ref, err := imageRef(source)
	if err != nil {
		return source // validate already rejected the shape; defensive only
	}
	return ref
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
