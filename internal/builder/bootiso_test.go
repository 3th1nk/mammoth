package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The d-i boot pair: isolinux gets the RAW kernel args (append has no ';'
// semantics — a grub-style escape would leak a literal backslash into the
// kernel command line), grub files get the escaped form.
func TestDebianBootConfigs(t *testing.T) {
	args := "auto=true priority=critical file=/cdrom/preseed.cfg ds=nocloud-net;s=file:///cdrom/"
	cfgs := debianBootConfigs(args, "install.amd")

	for _, want := range []string{
		"default mammoth",
		"kernel /install.amd/vmlinuz",
		"append initrd=/install.amd/initrd.gz " + args,
	} {
		if !strings.Contains(cfgs.isolinux, want) {
			t.Errorf("isolinux config missing %q:\n%s", want, cfgs.isolinux)
		}
	}
	if !strings.Contains(cfgs.grub, "linux /install.amd/vmlinuz") {
		t.Errorf("grub config missing linux line:\n%s", cfgs.grub)
	}
	if !strings.Contains(cfgs.grub, `ds=nocloud-net\;s=`) {
		t.Errorf("grub config must escape ';':\n%s", cfgs.grub)
	}
	if strings.Contains(cfgs.isolinux, `\;`) {
		t.Errorf("isolinux config must carry raw args (no grub escape):\n%s", cfgs.isolinux)
	}
	if !strings.Contains(cfgs.grub, "initrd /install.amd/initrd.gz") {
		t.Errorf("grub config missing initrd line:\n%s", cfgs.grub)
	}
}

// The layout → config map: debian-di covers the isolinux pair + grub, and
// patchBootConfigs only writes EFI/boot/grub.cfg when the image ships one.
func TestBootConfigsDebianLayout(t *testing.T) {
	cfgs := bootConfigs("auto=true priority=critical", layoutDebianDI, "install.amd")
	for _, rel := range []string{"isolinux/isolinux.cfg", "isolinux/txt.cfg", "boot/grub/grub.cfg", "EFI/boot/grub.cfg"} {
		if _, ok := cfgs[rel]; !ok {
			t.Errorf("debian-di configs missing %s", rel)
		}
	}
}

func TestBootConfigsCasperLayout(t *testing.T) {
	cfgs := bootConfigs("autoinstall ds=nocloud-net;s=file:///cdrom/", layoutCasper, "")
	for _, rel := range []string{"boot/grub/grub.cfg", "EFI/boot/grub.cfg"} {
		c, ok := cfgs[rel]
		if !ok {
			t.Errorf("casper configs missing %s", rel)
			continue
		}
		if !strings.Contains(c, "linux /casper/vmlinuz") || !strings.Contains(c, "initrd /casper/initrd") {
			t.Errorf("%s wrong kernel pair:\n%s", rel, c)
		}
		if !strings.Contains(c, `\;`) {
			t.Errorf("%s must escape ';':\n%s", rel, c)
		}
	}
}

func TestPatchBootConfigsOnlyExistingEFI(t *testing.T) {
	work := t.TempDir()
	// The image ships boot/grub/grub.cfg but no EFI/boot/grub.cfg (the
	// stock debian netinst shape).
	for _, rel := range []string{"isolinux/isolinux.cfg", "isolinux/txt.cfg", "boot/grub/grub.cfg"} {
		p := filepath.Join(work, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("stock"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := patchBootConfigs(work, "auto=true", layoutDebianDI, "install.amd"); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(work, "EFI", "boot", "grub.cfg")) {
		t.Errorf("EFI/boot/grub.cfg must not be created when the image has none")
	}
	b, err := os.ReadFile(filepath.Join(work, "boot", "grub", "grub.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "auto=true") {
		t.Errorf("boot/grub/grub.cfg not patched: %s", b)
	}
}

// -isohybrid-mbr: host path rewritten to the image's own copy; interval
// specs repoint at the absolute source path; dropped when neither exists.
func TestRewriteIsohybridMbr(t *testing.T) {
	work := t.TempDir()

	// host-side helper, no local isohdpfx.bin → dropped
	got := rewriteIsohybridMbr([]string{"-V", "X", "-isohybrid-mbr", "/usr/lib/ISOLINUX/isohdpfx.bin", "-b", "isolinux/isolinux.bin"}, work, "")
	want := []string{"-V", "X", "-b", "isolinux/isolinux.bin"}
	if len(got) != len(want) {
		t.Fatalf("drop path: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drop path: got %v, want %v", got, want)
		}
	}

	// host-side helper with a local copy → rewritten
	p := filepath.Join(work, "isolinux", "isohdpfx.bin")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("mbr"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = rewriteIsohybridMbr([]string{"-V", "X", "-isohybrid-mbr", "/usr/lib/ISOLINUX/isohdpfx.bin", "-b", "isolinux/isolinux.bin"}, work, "")
	if got[3] != p {
		t.Errorf("rewrite path: got %v, want mbr at %s", got, p)
	}

	// interval spec (debian-cd shape) with a RELATIVE source → absolute
	iv := "--interval:local_fs:0s-15s:zero_mbrpt,zero_gpt,zero_apm:debian-13.6.0-amd64-netinst.iso"
	got = rewriteIsohybridMbr([]string{"-isohybrid-mbr", iv, "-b", "isolinux/isolinux.bin"}, work, "/data/media/debian-13.6.0-amd64-netinst.iso")
	if got[1] != "--interval:local_fs:0s-15s:zero_mbrpt,zero_gpt,zero_apm:/data/media/debian-13.6.0-amd64-netinst.iso" {
		t.Errorf("interval repoint: got %v", got)
	}

	// interval spec, no source known → dropped
	got = rewriteIsohybridMbr([]string{"-isohybrid-mbr", iv, "-b", "isolinux/isolinux.bin"}, work, "")
	if len(got) != 2 {
		t.Errorf("interval drop: got %v", got)
	}
}
