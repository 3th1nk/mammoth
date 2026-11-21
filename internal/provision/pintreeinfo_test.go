package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedTreeVariantDirs materializes the Packages dirs a treeinfo variant
// layout claims, so the stub check sees real disk state.
func seedTreeVariantDirs(t *testing.T, root string, dirs map[string]int) {
	t.Helper()
	for dir, files := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < files; i++ {
			name := filepath.Join(root, filepath.FromSlash(dir), "pkg-"+strings.Repeat("x", i))
			if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Minimal media: BaseOS first with an EMPTY Packages dir (a stub) — the stub
// section must be dropped and the variants list pinned to the media's own
// default ([general] variant), which survives regardless of disk state.
func TestPinTreeinfoVariant(t *testing.T) {
	src := `[checksums]
images/pxeboot/vmlinuz = sha256:abc

[general]
arch = x86_64
family = Rocky Linux
packagedir = minimal/Packages
repository = minimal
variant = minimal
variants = BaseOS,minimal
version = 9.3

[stage2]
mainimage = images/install.img

[tree]
arch = x86_64
variants = BaseOS,minimal

[variant-BaseOS]
id = BaseOS
packages = BaseOS/Packages
repository = BaseOS
type = variant
uid = BaseOS

[variant-minimal]
id = minimal
packages = minimal/Packages
repository = minimal
type = variant
uid = minimal
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".treeinfo"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	seedTreeVariantDirs(t, dir, map[string]int{
		"BaseOS/Packages":  0, // exists and empty: the stub
		"minimal/Packages": 2,
	})
	if err := pinTreeinfoVariant(dir); err != nil {
		t.Fatalf("pin: %v", err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, ".treeinfo"))
	s := string(out)
	if !strings.Contains(s, "variants = minimal") || strings.Contains(s, "BaseOS,minimal") {
		t.Fatalf("variants not pinned:\n%s", s)
	}
	if strings.Contains(s, "[variant-BaseOS]") {
		t.Fatalf("stub variant section not dropped:\n%s", s)
	}
	// variant sections are left byte-identical (no arch injection — wire-
	// verified to break anaconda 9.3 variant resolution, 2288H)
	if strings.Contains(s, "[variant-minimal]\narch =") {
		t.Fatalf("arch must not be injected into variant sections:\n%s", s)
	}
	if !strings.Contains(s, "id = minimal") || !strings.Contains(s, "repository = minimal") {
		t.Fatalf("kept section body damaged:\n%s", s)
	}
}

// A full DVD ships BaseOS+AppStream with populated Packages dirs — both are
// load-bearing repos and the file must stay byte-identical. Pinning it down
// to the [general] variant deleted BaseOS and killed the round at the
// anaconda spoke ("Error setting up software source", 2026-11-22 rocky 9.8).
func TestPinTreeinfoVariantFullDVDTouched(t *testing.T) {
	src := `[checksums]
images/install.img = sha256:def

[general]
arch = x86_64
family = Rocky Linux
packagedir = AppStream/Packages
repository = AppStream
variant = AppStream
variants = AppStream,BaseOS
version = 9.8

[stage2]
mainimage = images/install.img

[tree]
arch = x86_64
variants = AppStream,BaseOS

[variant-AppStream]
id = AppStream
packages = AppStream/Packages
repository = AppStream
type = variant
uid = AppStream

[variant-BaseOS]
id = BaseOS
packages = BaseOS/Packages
repository = BaseOS
type = variant
uid = BaseOS
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".treeinfo"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	seedTreeVariantDirs(t, dir, map[string]int{
		"AppStream/Packages": 2,
		"BaseOS/Packages":    2,
	})
	if err := pinTreeinfoVariant(dir); err != nil {
		t.Fatalf("pin: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(dir, ".treeinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != src {
		t.Fatalf("full DVD treeinfo must stay byte-identical, got:\n%s", string(out))
	}
}

// A variant whose Packages dir is MISSING cannot be proven a stub (the dir
// layout may differ) — keep it rather than guess.
func TestPinTreeinfoVariantMissingPackagesKept(t *testing.T) {
	src := `[general]
variant = AppStream
variants = AppStream,BaseOS

[variant-AppStream]
id = AppStream
packages = AppStream/Packages
repository = AppStream
type = variant
uid = AppStream

[variant-BaseOS]
id = BaseOS
packages = BaseOS/Packages
repository = BaseOS
type = variant
uid = BaseOS
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".treeinfo"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only AppStream/Packages exists (populated); BaseOS/Packages is absent.
	seedTreeVariantDirs(t, dir, map[string]int{
		"AppStream/Packages": 1,
	})
	if err := pinTreeinfoVariant(dir); err != nil {
		t.Fatalf("pin: %v", err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, ".treeinfo"))
	if string(out) != src {
		t.Fatalf("unprovable stub must be kept, got:\n%s", string(out))
	}
}

// Single-variant layout: untouched, regardless of disk state.
func TestPinTreeinfoVariantSingleUntouched(t *testing.T) {
	src := `[general]
arch = x86_64
family = Rocky Linux
packagedir = minimal/Packages
repository = minimal
variant = minimal
variants = minimal
version = 9.3

[variant-minimal]
id = minimal
packages = minimal/Packages
repository = minimal
type = variant
uid = minimal
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".treeinfo"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// No Packages dirs on disk at all: nothing provable, file must survive.
	if err := pinTreeinfoVariant(dir); err != nil {
		t.Fatalf("pin: %v", err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, ".treeinfo"))
	if string(out) != src {
		t.Fatalf("single-variant treeinfo must stay untouched:\n%s", string(out))
	}
}
