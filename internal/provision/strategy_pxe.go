package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/builder"
	"github.com/3th1nk/mammoth/internal/netboot"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/render"
	"github.com/3th1nk/mammoth/internal/store"
)

// pxeStrategy boots the installer over the LAN: the distro ISO's boot files
// are extracted into a per-task HTTP boot tree, every NIC MAC of the
// machine is armed in the netboot registry, and the firmware is one-shot
// pointed at PXE. proxyDHCP + TFTP + iPXE (the netboot service) deliver the
// machine to the same kernel args the ISO path would bake in — the installer
// then pulls inst.repo / inst.ks exactly as before.
type pxeStrategy struct{ e *Executor }

func (s *pxeStrategy) name() bootStrategyName { return strategyPXE }

// prepare extracts the boot tree and registers the machine's NICs.
func (s *pxeStrategy) prepare(ctx context.Context, b *bootSession) error {
	e := s.e
	ictx, task := b.Ictx, b.Task
	driver, err := e.Render.For(b.Spec.Image.Distro)
	if err != nil {
		return classifiedErr("SCHEMA_UNKNOWN_DISTRO", false, "%s", err.Error())
	}
	carrier, pool := render.NetbootInstallOf(driver)
	distroISO, err := builder.EnsureISO(ctx, b.Spec.Image.Source, e.MediaDir)
	if err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"distro ISO fetch failed: %s", err.Error())
	}
	treeDir := filepath.Join(e.BootTreeDir, ictx.Token)
	var tree builder.BootTree
	switch carrier {
	case render.NetbootCarrierDINetboot:
		// The ISO's d-i initrd is the cdrom flavour — useless over the wire.
		// The official netboot tarball is the deployment-configured carrier;
		// its initrd pulls installer components from the HTTP pool.
		if e.PXEDINetbootTarball == "" {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", false,
				"%s PXE needs the d-i netboot tarball (set MAMMOTH_PXE_DI_NETBOOT) — the ISO's own initrd is the cdrom flavour and cannot fetch components over the network", b.Spec.Image.Distro)
		}
		tarball, terr := builder.EnsureISO(ctx, e.PXEDINetbootTarball, e.MediaWorkDir)
		if terr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"d-i netboot tarball fetch failed: %s", terr.Error())
		}
		tree, err = builder.ExtractDINetboot(ctx, tarball, treeDir)
		if err != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"boot tree extraction failed: %s", err.Error())
		}
	default:
		tree, err = builder.ExtractBootFiles(ctx, "", distroISO, treeDir)
		if err != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"boot tree extraction failed: %s", err.Error())
		}
	}
	// The unpacked install source rides the boot tree: HTTP-authorized for
	// pool consumers (d-i's mirror) or merely NFS-visible for NFS ones
	// (casper's netboot=nfs root, under the always-on MediaDir export).
	switch pool {
	case render.NetbootPoolHTTP, render.NetbootPoolNFS:
		if perr := builder.ExtractISOTree(ctx, "", distroISO, filepath.Join(treeDir, "iso")); perr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"install-source tree extraction failed: %s", perr.Error())
		}
		if pool == render.NetbootPoolHTTP {
			// Signed, udeb-complete offline mirror (Release re-checksummed and
			// re-signed; netboot udebs filled from the staged archive subset —
			// the netinst ISO prunes them, real-hardware: anna died with "No
			// kernel modules were found" and apt-setup rejected the unsigned
			// pool; see builder.StageNetbootPool).
			ent, kerr := builder.PoolSigningEntity(e.MediaDir)
			if kerr != nil {
				return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", false,
					"pool signing key unavailable: %s", kerr.Error())
			}
			if serr := builder.StageNetbootPool(filepath.Join(treeDir, "iso"), e.PXEDIUdebsDir, ent); serr != nil {
				return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
					"pool staging failed: %s", serr.Error())
			}
			tree.Extra["iso"] = "dir:iso"
		}
	}

	// Arm every inventoried NIC: whichever the firmware actually boots is
	// a firmware-order decision mammoth doesn't see; UNIQUE(mac) keeps one
	// pending entry per NIC and release is by task.
	m, err := e.Machines.Get(ctx, task.MachineID)
	if err != nil {
		return err
	}
	var hw bmc.HardwareView
	_ = json.Unmarshal(m.Hardware, &hw)
	macs := macsFor(hw, b.Spec.Network)
	if len(macs) == 0 {
		return classifiedErr("NETBOOT_MAC_UNAVAILABLE", false,
			"machine %s has no NIC MAC in its inventory (re-run discover with redfish or inband_ssh) — PXE boot entries are keyed by MAC", task.MachineID)
	}
	for _, mac := range macs {
		args := b.Boot.KernelArgs
		if b.Boot.NetbootKernelArgs != "" {
			args = b.Boot.NetbootKernelArgs
		}
		entry := &store.NetbootEntry{
			MAC: mac, TaskID: task.ID, MachineID: task.MachineID, Token: ictx.Token,
			Kind: "install", Kernel: tree.Kernel, Initrd: tree.Initrd,
			KernelArgs: args, Extra: tree.Extra,
		}
		if err := e.Netboot.Upsert(ctx, entry); err != nil {
			return classifiedErr("NETBOOT_REGISTER_FAILED", true,
				"netboot entry for %s: %s", mac, err.Error())
		}
	}
	ictx.BootStrategy = string(strategyPXE)
	ictx.Netboot = &netbootRecord{Token: ictx.Token, MACs: macs}
	e.Events.Append(ctx, "task", task.ID, "task.netboot_registered", map[string]any{
		"macs": macs, "kind": "install",
	})
	obs.FromContext(ctx).InfoContext(ctx, "netboot tree armed",
		"macs", len(macs), "kernel", tree.Kernel, "initrd", tree.Initrd,
		"kernel_args", b.Boot.NetbootKernelArgs)
	return nil
}

// nfsRootFor converts the NFS media base (nfs://host/export) into casper's
// nfsroot form (host:/export/netboot/<token>/iso — the colon separator is
// what busybox nfsmount parses server from path; without it the mount
// reports "need a path"). Empty base → empty result: the NFS-pool drivers
// reject it at render/prepare time.
func nfsRootFor(mediaNFSBase, token string) string {
	if mediaNFSBase == "" || token == "" {
		return ""
	}
	u := strings.TrimPrefix(mediaNFSBase, "nfs://")
	u = strings.TrimSuffix(u, "/")
	host, export, ok := strings.Cut(u, "/")
	if !ok {
		return ""
	}
	return host + ":/" + export + "/netboot/" + token + "/iso"
}

// arm one-shot points the firmware at PXE and powers the machine. No media
// mount, no settle delay — the boot tree is already complete on this host's
// disk, so there is no out-of-band transfer tail to wait out.
func (s *pxeStrategy) arm(ctx context.Context, b *bootSession) error {
	e := s.e
	task := b.Task
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootPXE, true)
	}); err != nil {
		return err
	}
	return powerIntoInstaller(ctx, e, task, true)
}

// release disarms the registry rows and removes the boot tree.
func (s *pxeStrategy) release(ctx context.Context, b *bootSession, reason string) {
	s.e.releasePXE(ctx, b.Task.ID, b.Ictx.Netboot.NetbootToken(), reason)
}

// macsFor collects the machine's PXE-able MACs: normalized inventory NICs,
// plus any MAC the spec's network entries match on (a static-net declaration
// names the NICs the operator cares about; it must never be narrower than
// what actually boots, so its MACs only ADD candidates).
func macsFor(hw bmc.HardwareView, network []networkView) []string {
	seen := map[string]bool{}
	var out []string
	add := func(mac string) {
		mac = netboot.NormalizeMAC(mac)
		if mac != "" && !seen[mac] {
			seen[mac] = true
			out = append(out, mac)
		}
	}
	for _, n := range network {
		if n.Match != nil {
			add(n.Match.MAC)
		}
	}
	for _, nic := range hw.NICs {
		add(nic.MAC)
	}
	return out
}

// NetbootToken is the boot-tree directory name (nil-safe).
func (r *netbootRecord) NetbootToken() string {
	if r == nil {
		return ""
	}
	return r.Token
}

// removeBootTree deletes a per-task boot tree under root (best effort).
func removeBootTree(root, token string) {
	if token == "" || token == "." || token == ".." || strings.ContainsAny(token, "/\\") {
		return
	}
	_ = os.RemoveAll(filepath.Join(root, token))
}
