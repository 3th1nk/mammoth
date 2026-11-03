package provision

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/3th1nk/mammoth/internal/builder"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/render"
)

// The shared pool store: one content-addressed unpacked-ISO tree per distro
// image, reused by every install task instead of a fresh ~2.5G extraction
// per task (legacy ⑤ — batch installs of one distro paid the extraction N
// times and the boot-tree volume filled three runs in a day). The tree lives
// under MediaDir/pool-store/<sha256>/iso, OUTSIDE BootTreeDir: per-task trees
// (kernel/initrd) still die with their task, while a shared tree outlives
// tasks until the startup TTL sweep reclaims it. HTTP consumers fetch it via
// /netboot/store/<sha>/…, NFS consumers mount the same path through the
// MediaDir export — no symlink anywhere (the NFS mount protocol's symlink
// handling is not something to bet a real-machine install on).
//
// Visible means complete: a tree is extracted and (for the d-i HTTP pool)
// pool-staged inside a temp directory and atomically renamed into place, so
// reuse never sees a half-built tree and never re-stages — which matters
// because StageNetbootPool MUTATES the tree (udeb fill, Release rewrite,
// signature). Changing the pool signing key or the udeb archive therefore
// requires deleting the affected store entry by hand; both are rare,
// documented deployment events.

const (
	// PoolStoreDirName is the shared tree root under MediaDir.
	PoolStoreDirName = "pool-store"
	// poolStoreTTL bounds a shared tree's life without a touch: startup
	// sweeps older entries (an idle distro tree costs ~2.5G). Installs
	// touch the tree they use, so anything swept has been unused for the
	// full TTL — far beyond any install task's lifetime.
	poolStoreTTL = 7 * 24 * time.Hour
	// poolStoreMinFree is the free-space floor for a FIRST extraction
	// (reuse needs nothing): the same 3GiB the per-task boot tree requires.
	poolStoreMinFree = 3 << 30
)

// poolStoreMu serializes first extractions across concurrent tasks (a batch
// of same-distro machines otherwise races N extractions into one address).
var poolStoreMu sync.Mutex

// EnsurePoolTree makes the shared, content-addressed unpack of isoPath
// available under MediaDir/pool-store/<sha>/iso and returns the store path.
// Reuse only touches the tree's mtime; first build extracts and — for the
// d-i HTTP pool — stages the signed mirror. extract allows tests to stand in
// for the xorriso-backed extraction.
func EnsurePoolTree(ctx context.Context, e *Executor, isoPath, sha string, pool render.NetbootPool, extract func(ctx context.Context, isoPath, destDir string) error) (string, error) {
	storeDir := filepath.Join(e.MediaDir, PoolStoreDirName, sha)
	final := filepath.Join(storeDir, "iso")

	poolStoreMu.Lock()
	defer poolStoreMu.Unlock()

	if _, err := os.Stat(final); err == nil {
		now := time.Now()
		_ = os.Chtimes(final, now, now) // liveness for the startup TTL sweep
		return final, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// First build pays the disk: refuse before moving gigabytes into a full
	// volume (the per-task boot tree got this guard after the third
	// disk-full killed a run mid-flight).
	if err := poolStoreHeadroom(e.MediaDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(e.MediaDir, PoolStoreDirName), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Join(e.MediaDir, PoolStoreDirName), ".extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if extract == nil {
		extract = func(ctx context.Context, iso, dest string) error {
			return builder.ExtractISOTree(ctx, "", iso, dest)
		}
	}
	if err := extract(ctx, isoPath, filepath.Join(tmp, "iso")); err != nil {
		return "", fmt.Errorf("pool store extraction failed: %w", err)
	}
	if pool == render.NetbootPoolPlain {
		// RHEL-lineage minimal media ship multi-variant treeinfos whose
		// variant sections lack the arch field anaconda matches by, and the
		// first variant can be a stub with an empty Packages dir — either
		// way anaconda dies on "Error setting up software source" (2288H
		// real-hardware). Pin the tree to the media's own default variant
		// ([general] variant) with the arch injected.
		if terr := pinTreeinfoVariant(filepath.Join(tmp, "iso")); terr != nil {
			return "", fmt.Errorf("treeinfo variant pin failed: %w", terr)
		}
	}
	if pool == render.NetbootPoolHTTP {
		// Signed, udeb-complete d-i mirror (see StageNetbootPool). The key is
		// instance-stable (MediaDir-held, first-use generated).
		ent, kerr := builder.PoolSigningEntity(e.MediaDir)
		if kerr != nil {
			return "", fmt.Errorf("pool signing key unavailable: %w", kerr)
		}
		if serr := builder.StageNetbootPool(filepath.Join(tmp, "iso"), e.PXEDIUdebsDir, ent); serr != nil {
			return "", fmt.Errorf("pool staging failed: %w", serr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(storeDir), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, storeDir); err != nil {
		// A concurrent first build won the rename (POSIX: target exists and
		// is non-empty) — adopt the winner's tree, it is byte-identical by
		// construction (same content address).
		if _, serr := os.Stat(final); serr == nil {
			return final, nil
		}
		return "", err
	}
	obs.FromContext(ctx).InfoContext(ctx, "pool store built",
		"sha256", sha, "path", final)
	return final, nil
}

// poolStoreHeadroom refuses the first extraction when the store volume is
// low — with directions that fit the SHARED tree (a per-task sweep cannot
// reclaim it; the operator deletes unused distro trees by hand, and they
// re-extract on the next install).
func poolStoreHeadroom(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil // unknown volume state: let the build try and fail honestly
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free < poolStoreMinFree {
		return fmt.Errorf("pool-store volume is low on space (%dMiB free, need %dMiB) — remove unused distro trees under %s (they re-extract on the next install of that image)",
			free>>20, poolStoreMinFree>>20, filepath.Join(dir, PoolStoreDirName))
	}
	return nil
}

// SweepPoolStore removes shared trees untouched for the TTL (startup-time
// garbage collection: a crashed runner never per-task-owns a shared tree,
// so age since last use is the only lifecycle signal there is). Best effort.
func SweepPoolStore(mediaDir string, log *slog.Logger) {
	root := filepath.Join(mediaDir, PoolStoreDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no store yet
	}
	deadline := time.Now().Add(-poolStoreTTL)
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.HasPrefix(name, ".") {
			continue // temp extraction dirs: in-flight or crashed, leave them
		}
		dir := filepath.Join(root, name)
		fi, err := os.Stat(filepath.Join(dir, "iso"))
		if err != nil {
			continue // not a (complete) store entry
		}
		if fi.ModTime().After(deadline) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			log.Warn("pool store sweep failed", "dir", dir, "err", err.Error())
			continue
		}
		log.Info("pool store entry swept (unused past TTL)", "dir", dir)
	}
}

// pinTreeinfoVariant rewrites a multi-variant .treeinfo down to the media's
// own default variant ([general] variant): the [tree] variants list is
// pinned to that single variant and the other [variant-*] sections are
// dropped (minimal media list BaseOS first with an empty Packages dir —
// anaconda resolves the source to it and dies on "Error setting up software
// source", real-hardware 2288H). Variant sections are otherwise left
// untouched — wire-verified: injecting an arch line the media itself does
// not carry breaks anaconda's variant resolution on 9.3 (2026-11-03).
// No .treeinfo (non-RHEL media) is a no-op.
func pinTreeinfoVariant(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, ".treeinfo"))
	if err != nil {
		return nil // not RHEL-lineage media: nothing to pin
	}
	lines := strings.Split(string(raw), "\n")
	variant := ""
	inGeneral := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "["):
			inGeneral = t == "[general]"
		case inGeneral && strings.HasPrefix(t, "variant"):
			key, v, _ := strings.Cut(t, "=")
			if strings.TrimSpace(key) == "variant" && strings.TrimSpace(v) != "" {
				variant = strings.TrimSpace(v)
			}
		}
	}
	if variant == "" {
		return nil // single/unknown variant layout: the treeinfo is fine as-is
	}
	keep := func(t string) bool {
		return !strings.HasPrefix(t, "[variant-") || t == "[variant-"+variant+"]"
	}
	var b strings.Builder
	inDropped := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			inDropped = !keep(t)
			if !inDropped {
				b.WriteString(t)
				b.WriteString("\n")
			}
			continue
		}
		if inDropped {
			continue
		}
		if strings.HasPrefix(t, "variants") {
			if _, _, ok := strings.Cut(t, "="); ok {
				ln = "variants = " + variant
			}
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return os.WriteFile(filepath.Join(root, ".treeinfo"), []byte(b.String()), 0o644)
}
