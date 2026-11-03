package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	// single-variant layout: untouched
	single := strings.ReplaceAll(src, "variants = BaseOS,minimal", "variants = minimal")
	single = strings.Replace(single, `[variant-BaseOS]
id = BaseOS
packages = BaseOS/Packages
repository = BaseOS
type = variant
uid = BaseOS

`, "", 1)
	if err := os.WriteFile(filepath.Join(dir, ".treeinfo"), []byte(single), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pinTreeinfoVariant(dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".treeinfo"))
	if strings.TrimRight(string(after), "\n") != strings.TrimRight(single, "\n") {
		t.Fatalf("single-variant treeinfo must stay untouched:\n%s", string(after))
	}
}
