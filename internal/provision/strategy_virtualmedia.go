package provision

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/3th1nk/mammoth/internal/bmc"
	"github.com/3th1nk/mammoth/internal/builder"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// virtualMediaStrategy is the original boot carrier: builder repacks the
// distro ISO into boot-<token>.iso, the BMC mounts it through the
// BMC-reachable share, and a one-shot CD override lands the installer.
// The bodies are the pre-strategy prepareMedia/bootStage code, moved.
type virtualMediaStrategy struct{ e *Executor }

func (s *virtualMediaStrategy) name() bootStrategyName { return strategyVirtualMedia }

// prepare assembles the per-task boot ISO (media B) from THIS render pass —
// building from the previous pass's ictx.Answers produced a seed-less ISO
// and a stalled subiquity on real hardware (see prepareMedia notes).
func (s *virtualMediaStrategy) prepare(ctx context.Context, b *bootSession) error {
	e := s.e
	ictx := b.Ictx
	mediaFile := fmt.Sprintf("boot-%s.iso", ictx.Token)
	mediaURI := mediaURIFor(e.MediaBaseURI, filepath.Base(mediaFile))
	distroISO, err := builder.EnsureISO(ctx, b.Spec.Image.Source, e.MediaDir)
	if err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"distro ISO fetch failed: %s", err.Error())
	}
	seed := map[string]string{}
	for _, a := range b.Answers {
		seed[a.Name] = a.Content
	}
	for name, content := range b.Seed {
		seed[name] = content
	}
	// Build in the scratch space when configured (the media repo may be a
	// size-limited share — the extract+assemble needs ~2x the image size
	// transiently), then move the finished image into the repo. Precheck the
	// space and fail fast with real numbers: a mid-write ENOSPC surfaces as
	// an opaque xorriso SORRY/excess-space error instead (real-hardware: an
	// 18G disk filled by accumulated media produced opaque build failures).
	isoStat, err := os.Stat(distroISO)
	if err != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true, "distro ISO missing: %s", err.Error())
	}
	needMB := isoStat.Size()/1048576*2 + 512 // extract + assemble + headroom
	if layout, _, lerr := builder.DetectLayout(ctx, "", distroISO); lerr == nil && !layout.FullRepack() {
		needMB = 2560
	}
	outputPath := filepath.Join(e.MediaDir, filepath.Base(mediaFile))
	buildWork := ""
	buildDir := e.MediaWorkDir
	if buildDir == "" {
		buildDir = filepath.Dir(outputPath)
	}
	if e.MediaWorkDir != "" {
		buildWork = filepath.Join(e.MediaWorkDir, filepath.Base(mediaFile)+".build")
		outputPath = filepath.Join(e.MediaWorkDir, filepath.Base(mediaFile))
	}
	if avail, ok := freeMB(buildDir); ok && avail < needMB {
		return classifiedErr("MEDIA_NO_SPACE", true,
			"media build needs ~%d MB free in %s, %d MB available: clear old ISOs or extend the volume",
			needMB, buildDir, avail)
	}
	if avail, ok := freeMB(e.MediaDir); ok && avail < int64(isoStat.Size()/1048576) {
		return classifiedErr("MEDIA_NO_SPACE", true,
			"media repo %s needs ~%d MB free for the boot ISO, %d MB available",
			e.MediaDir, isoStat.Size()/1048576, avail/1048576)
	}
	if berr := buildBootISO(ctx, distroISO, outputPath, b.Boot.KernelArgs, seed, buildWork); berr != nil {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
			"boot media build failed: %s", berr.Error())
	}
	if buildWork != "" {
		if merr := moveFile(outputPath, filepath.Join(e.MediaDir, filepath.Base(mediaFile))); merr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"boot media move into repo failed: %s", merr.Error())
		}
	}
	// Relay deployment: the BMC mounts the media from the remote export —
	// the file must be complete there BEFORE the boot stage mounts it. The
	// push is synchronous and atomic (temp name + rename).
	if e.MediaUploader != nil {
		if _, perr := e.MediaUploader.Push(ctx, filepath.Join(e.MediaDir, filepath.Base(mediaFile))); perr != nil {
			return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", true,
				"boot media relay push failed: %s", perr.Error())
		}
		obs.FromContext(ctx).InfoContext(ctx, "boot media relayed", "file", filepath.Base(mediaFile))
	}
	ictx.MediaURI = mediaURI
	obs.FromContext(ctx).InfoContext(ctx, "boot media built",
		"media_uri", mediaURI, "kernel_args", b.Boot.KernelArgs)
	return nil
}

// arm mounts the boot ISO and one-shot boots the machine from CD.
func (s *virtualMediaStrategy) arm(ctx context.Context, b *bootSession) error {
	e := s.e
	task := b.Task
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}

	// Mount the builder's boot ISO (media B) — the BMC fetches it via NFS.
	// Eject any existing media first (the slot may be occupied from a
	// previous task or mount_media action).
	bootMedia := bmc.MediaImage{URL: b.Ictx.MediaURI, Kind: bmc.MediaBoot}
	if bootMedia.URL == "" {
		return classifiedErr("INSTALL_MEDIA_BUILD_FAILED", false,
			"boot ISO URI is empty; prepare_media must run first")
	}
	_, _ = e.BMC.Do(ctx, addr, cred, proto, "eject_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.EjectMedia(ctx, addr, cred, bootMedia)
	})
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "mount_media", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.MountMedia(ctx, addr, cred, bootMedia)
	}); err != nil {
		return err
	}
	// Media settle delay: BMC virtual-media mounts can succeed while the
	// backing file is still arriving (NFS relay deployments — the mount only
	// validates the image header; the firmware reads the payload much later,
	// and a partial read means a dead CD boot). Wait out the transfer.
	if settle := e.BootSettleDelay; settle > 0 {
		obs.FromContext(ctx).InfoContext(ctx, "waiting for boot media to settle",
			"delay", settle.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settle):
		}
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_boot_device", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetBootDevice(ctx, addr, cred, bmc.BootCDROM, true)
	}); err != nil {
		return err
	}
	return powerIntoInstaller(ctx, e, task, true)
}

// release reclaims the media: on the completed path eject FIRST (the
// machine may reboot back into a still-mounted installer ISO), then the
// file sweep; terminal paths skip the eject — the one-shot override has
// long been consumed and cleanupBootMedia semantics are preserved.
func (s *virtualMediaStrategy) release(ctx context.Context, b *bootSession, reason string) {
	e := s.e
	if reason == reasonCompleted {
		e.ejectBootMediaBestEffort(ctx, b.Task, b.Ictx)
	}
	e.cleanupBootMedia(ctx, b.Task, reason)
}

// powerIntoInstaller is the shared tail of every arm: cycle if on,
// power-on if off, then (install flow only — the probe context carries no
// install record) record booted_at.
func powerIntoInstaller(ctx context.Context, e *Executor, task *store.Task, recordProgress bool) error {
	cred, addr, proto, ok := e.outOfBand(ctx, task)
	if !ok {
		return classifiedErr("CREDENTIAL_UNAVAILABLE", true, "machine or credential unavailable")
	}
	ps, err := e.BMC.Do(ctx, addr, cred, proto, "power_state", func(ctx context.Context, d bmc.Driver) (any, error) {
		return d.PowerState(ctx, addr, cred)
	})
	action := bmc.PowerOn
	if err == nil && ps.(bmc.PowerState) == bmc.PowerStateOn {
		action = bmc.Cycle
	}
	if _, err := e.BMC.Do(ctx, addr, cred, proto, "set_power", func(ctx context.Context, d bmc.Driver) (any, error) {
		return nil, d.SetPower(ctx, addr, cred, action)
	}); err != nil {
		return err
	}
	if recordProgress {
		now := time.Now().UTC()
		if err := e.Jobs.RecordInstallProgress(ctx, task.ID, map[string]any{"booted_at": now}); err != nil {
			return err
		}
	}
	obs.FromContext(ctx).InfoContext(ctx, "machine booted into installer",
		"action", string(action))
	return nil
}
