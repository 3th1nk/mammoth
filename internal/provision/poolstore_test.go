package provision

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/3th1nk/mammoth/internal/render"
)

// fakeExtract stands in for the xorriso-backed tree extraction (which
// creates destDir itself): it writes a marker so tests can see the tree
// content landed.
func fakeExtract(marker string) (func(ctx context.Context, isoPath, destDir string) error, *int) {
	calls := 0
	return func(ctx context.Context, isoPath, destDir string) error {
		calls++
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destDir, "kernel.img"), []byte(marker), 0o644)
	}, &calls
}

func poolStoreExecutor(t *testing.T) *Executor {
	t.Helper()
	return &Executor{MediaDir: t.TempDir()}
}

// EnsurePoolTree builds once and reuses: the second call for the same
// content address must not extract again (the whole point of the store).
func TestEnsurePoolTreeBuildOnce(t *testing.T) {
	e := poolStoreExecutor(t)
	extract, calls := fakeExtract("tree-bytes")

	first, err := EnsurePoolTree(context.Background(), e, "/tmp/unused.iso",
		"aa", render.NetbootPoolNFS, extract)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("extract calls after first build = %d", *calls)
	}
	if _, err := os.Stat(filepath.Join(first, "kernel.img")); err != nil {
		t.Fatalf("tree content missing: %v", err)
	}

	second, err := EnsurePoolTree(context.Background(), e, "/tmp/unused.iso",
		"aa", render.NetbootPoolNFS, extract)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if second != first || *calls != 1 {
		t.Fatalf("reuse re-extracted: path=%s calls=%d", second, *calls)
	}
}

// The extraction failing must leave no half-built tree behind: reuse sees
// "not there", never "there but incomplete" (visible means complete).
func TestEnsurePoolTreeFailureLeavesNothing(t *testing.T) {
	e := poolStoreExecutor(t)
	extract, _ := fakeExtract("tree-bytes")
	if _, err := EnsurePoolTree(context.Background(), e, "iso", "bb",
		render.NetbootPoolNFS, func(ctx context.Context, iso, dest string) error {
			if err := extract(ctx, iso, dest); err != nil {
				return err
			}
			return errors.New("xorriso died half-way")
		}); err == nil {
		t.Fatal("want extraction failure")
	}
	if _, err := os.Stat(filepath.Join(e.MediaDir, PoolStoreDirName, "bb", "iso")); !os.IsNotExist(err) {
		t.Fatalf("half-built tree visible: %v", err)
	}
	// The temp extraction dir is cleaned too (a crashed extraction must not
	// leak gigabytes into the store).
	entries, _ := os.ReadDir(filepath.Join(e.MediaDir, PoolStoreDirName))
	for _, en := range entries {
		if len(en.Name()) > 0 && en.Name()[0] == '.' {
			t.Fatalf("temp extraction leaked: %s", en.Name())
		}
	}
}

// A concurrent build that wins the rename is adopted by the loser: the
// content address guarantees byte-identical trees.
func TestEnsurePoolTreeAdoptsWinner(t *testing.T) {
	e := poolStoreExecutor(t)
	storeDir := filepath.Join(e.MediaDir, PoolStoreDirName, "cc")
	if err := os.MkdirAll(filepath.Join(storeDir, "iso"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "iso", "kernel.img"), []byte("winner"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The rename to an occupied (non-empty) target fails; the adopt path
	// must return the winner's tree, not the error.
	got, err := EnsurePoolTree(context.Background(), e, "iso", "cc",
		render.NetbootPoolNFS, func(ctx context.Context, iso, dest string) error {
			return nil
		})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(got, "kernel.img"))
	if string(b) != "winner" {
		t.Fatalf("winner tree not adopted: %q", b)
	}
}

// SweepPoolStore reclaims trees untouched past the TTL and nothing else:
// fresh trees, in-flight extractions, and non-tree entries survive.
func TestSweepPoolStore(t *testing.T) {
	media := t.TempDir()
	logger := discardTestLogger()
	mk := func(sha string, age time.Duration) {
		t.Helper()
		dir := filepath.Join(media, PoolStoreDirName, sha)
		if err := os.MkdirAll(filepath.Join(dir, "iso"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(dir, "iso"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	mk("old", 8*24*time.Hour)
	mk("new", time.Hour)
	// In-flight extraction (crashed or running): never swept by content rules.
	if err := os.MkdirAll(filepath.Join(media, PoolStoreDirName, ".extract-x", "iso"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(media, PoolStoreDirName, ".extract-x"), old, old); err != nil {
		t.Fatal(err)
	}
	// Not a (complete) tree: no iso inside.
	if err := os.MkdirAll(filepath.Join(media, PoolStoreDirName, "stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	SweepPoolStore(media, logger)

	for _, kept := range []string{"new", ".extract-x", "stub"} {
		if _, err := os.Stat(filepath.Join(media, PoolStoreDirName, kept)); err != nil {
			t.Errorf("%s must survive the sweep: %v", kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(media, PoolStoreDirName, "old")); !os.IsNotExist(err) {
		t.Errorf("stale tree must be swept: %v", err)
	}
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The casper nfsroot form: export-relative path under the media base.
func TestNFSRootForRelPath(t *testing.T) {
	got := nfsRootFor("nfs://198.51.100.248/data/media", filepath.Join(PoolStoreDirName, "ab12", "iso"))
	if got != "198.51.100.248:/data/media/"+PoolStoreDirName+"/ab12/iso" {
		t.Fatalf("nfsRootFor = %q", got)
	}
	if nfsRootFor("", "x") != "" || nfsRootFor("nfs://host", "x") != "" {
		t.Fatal("empty base or export must yield empty result")
	}
}
