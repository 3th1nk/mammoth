package netboot

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestRenderScript(t *testing.T) {
	e := &Entry{
		MAC:        "52:54:00:12:34:56",
		TaskID:     "tsk_1",
		Kind:       "install",
		Token:      "tok9",
		Kernel:     "vmlinuz",
		Initrd:     "initrd.img",
		KernelArgs: "inst.ks=http://192.168.77.1:8080/render/tok9/ks.cfg  inst.repo=nfs:192.168.77.1:/media ip=dhcp",
	}
	got := RenderScript(e, "http://192.168.77.1:8080/")
	want := strings.Join([]string{
		"#!ipxe",
		"# mammoth boot entry mac=52:54:00:12:34:56 task=tsk_1 kind=install",
		"kernel http://192.168.77.1:8080/netboot/files/tok9/vmlinuz inst.ks=http://192.168.77.1:8080/render/tok9/ks.cfg inst.repo=nfs:192.168.77.1:/media ip=dhcp",
		"initrd http://192.168.77.1:8080/netboot/files/tok9/initrd.img",
		"boot",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("script mismatch:\n%s\nwant:\n%s", got, want)
	}

	// Trailing slash in the base URL must not double up; whitespace runs in
	// the kernel args collapse to single spaces.
	e.KernelArgs = "a  \t b\nc"
	if s := RenderScript(e, "http://h:1"); strings.Contains(s, "a b c") == false {
		t.Errorf("args not sanitized: %q", s)
	}
}

func TestNoEntryScriptExits(t *testing.T) {
	s := NoEntryScript("52:54:00:12:34:56")
	if !strings.HasPrefix(s, "#!ipxe") || !strings.Contains(s, "exit") {
		t.Fatalf("fallback must exit back to firmware: %q", s)
	}
}

func TestRenderEnrollScript(t *testing.T) {
	e := &Entry{Token: "enroll", Kind: "probe", Kernel: "vmlinuz", Initrd: "initrd.img",
		KernelArgs: "modules=loop,squashfs ip=dhcp modloop=http://m:8080/netboot/enroll-file/modloop",
		Extra:      map[string]string{"modloop": "modloop"}}
	s := RenderEnrollScript(e, "http://10.0.0.1:8080", "52:54:00:12:34:56")
	if !strings.HasPrefix(s, "#!ipxe") {
		t.Fatalf("not an iPXE script: %q", s)
	}
	// The enroll MAC rides the kernel line — the shared overlay reads it off
	// /proc/cmdline and keys the report with it.
	if !strings.Contains(s, "enroll_mac=52:54:00:12:34:56") {
		t.Errorf("script missing enroll_mac: %q", s)
	}
	// Files come from the shared tree endpoint, not a per-task token dir.
	if !strings.Contains(s, "/netboot/enroll-file/vmlinuz") ||
		!strings.Contains(s, "/netboot/enroll-file/initrd.img") {
		t.Errorf("script must fetch from the enroll tree: %q", s)
	}
	if strings.Contains(s, "\n\n") || strings.Count(s, "\n") > 6 {
		t.Errorf("script shape drifted: %q", s)
	}
}

func TestGrantedSubtrees(t *testing.T) {
	e := &Entry{Kernel: "vmlinuz", Initrd: "initrd", Extra: map[string]string{
		"modloop": "modloop", "apks": "dir:apks", "empty": "dir:",
	}}
	if got := e.GrantedSubtrees(); len(got) != 1 || got[0] != "apks" {
		t.Errorf("granted subtrees = %v, want [apks]", got)
	}
}

func TestFileNamesAllowlist(t *testing.T) {
	e := &Entry{Kernel: "vmlinuz", Initrd: "initrd", Extra: map[string]string{"modloop": "modloop-lts"}}
	names := map[string]bool{}
	for _, n := range e.AllowlistedFiles() {
		names[n] = true
	}
	for _, want := range []string{"vmlinuz", "initrd", "modloop-lts"} {
		if !names[want] {
			t.Errorf("allowlist missing %q: %v", want, names)
		}
	}
}

func TestCachedResolver(t *testing.T) {
	calls := 0
	inner := ResolverFunc(func(_ context.Context, mac string) (*Entry, error) {
		calls++
		if mac == "err" {
			return nil, context.DeadlineExceeded
		}
		return &Entry{MAC: mac}, nil
	})
	c := NewCachedResolver(inner, time.Minute)

	for range 5 {
		e, err := c.Entry(context.Background(), "52:54:00:12:34:56")
		if err != nil || e == nil || e.MAC != "52:54:00:12:34:56" {
			t.Fatalf("entry = %+v %v", e, err)
		}
	}
	if calls != 1 {
		t.Fatalf("cache missed: %d calls for 5 lookups", calls)
	}
	// Errors are cached too — a flapping store must not hammer lookups.
	for range 3 {
		if _, err := c.Entry(context.Background(), "err"); err == nil {
			t.Fatal("want error passthrough")
		}
	}
	if calls != 2 {
		t.Fatalf("error not cached: %d calls", calls)
	}
}

func TestCachedResolverTTLExpiry(t *testing.T) {
	calls := 0
	inner := ResolverFunc(func(context.Context, string) (*Entry, error) {
		calls++
		return nil, nil
	})
	c := NewCachedResolver(inner, 10*time.Millisecond)
	_, _ = c.Entry(context.Background(), "a")
	time.Sleep(15 * time.Millisecond)
	_, _ = c.Entry(context.Background(), "a")
	if calls != 2 {
		t.Fatalf("TTL not honored: %d calls", calls)
	}
}

func TestNBPPresenceGate(t *testing.T) {
	s := &Server{
		opts: Options{
			NBPs: fstest.MapFS{"ipxe-amd64.efi": &fstest.MapFile{Data: []byte("x")}},
		},
		nbpOK: map[string]bool{},
	}
	if !s.hasNBP("ipxe-amd64.efi") {
		t.Error("present NBP reported missing")
	}
	if s.hasNBP("undionly.kpxe") {
		t.Error("missing NBP reported present")
	}
	if nbpFor(ArchIA32) != "" {
		t.Error("ia32 must have no NBP (falls through)")
	}
	if nbpFor(ArchX64) != "shimx64.efi" {
		t.Errorf("x64 NBP = %q, want the shim chain", nbpFor(ArchX64))
	}
	if nbpFor(ArchARM64) != "shimaa64.efi" {
		t.Errorf("arm64 NBP = %q, want the shim chain", nbpFor(ArchARM64))
	}
}

func TestRenderGRUB(t *testing.T) {
	e := &Entry{
		MAC:        "52:54:00:12:34:56",
		TaskID:     "tsk_1",
		Kind:       "install",
		Token:      "tok9",
		Kernel:     "vmlinuz",
		Initrd:     "initrd.img",
		KernelArgs: "inst.ks=http://192.168.77.1:8080/render/tok9/ks.cfg ip=dhcp",
	}
	got := RenderGRUB(e, "http://192.168.77.1:8080/")
	for _, want := range []string{
		"linux (http,192.168.77.1:8080)/netboot/files/tok9/vmlinuz inst.ks=http://192.168.77.1:8080/render/tok9/ks.cfg ip=dhcp",
		"initrd (http,192.168.77.1:8080)/netboot/files/tok9/initrd.img",
		"boot",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("grub.cfg missing %q:\n%s", want, got)
		}
	}
}

func TestNoEntryGRUBExits(t *testing.T) {
	s := NoEntryGRUB("52:54:00:12:34:56")
	if !strings.Contains(s, "exit") {
		t.Fatalf("fallback must exit back to firmware: %q", s)
	}
}

func TestGRUBConfigMAC(t *testing.T) {
	if mac, ok := grubConfigMAC("grub.cfg-01-52:54:00:12:34:56"); !ok || mac != "52:54:00:12:34:56" {
		t.Errorf("grub.cfg-01-MAC: %q %v", mac, ok)
	}
	// Mixed-case / alternate separators normalize to the canonical lowercase
	// colon form (the key the resolver expects).
	if mac, ok := grubConfigMAC("grub.cfg-01-52-54-00-12-34-56"); !ok || mac != "52:54:00:12:34:56" {
		t.Errorf("normalize: %q %v", mac, ok)
	}
	if _, ok := grubConfigMAC("undionly.kpxe"); ok {
		t.Error("non-grub filename must not match")
	}
	if _, ok := grubConfigMAC("grub.cfg"); ok {
		t.Error("bare grub.cfg must not match the per-MAC pattern")
	}
}

func TestGRUBHTTPHost(t *testing.T) {
	if h := grubHTTPHost("http://192.168.77.1:8080"); h != "192.168.77.1:8080" {
		t.Errorf("host with port = %q", h)
	}
	if h := grubHTTPHost("http://192.168.77.1/"); h != "192.168.77.1" {
		t.Errorf("host without port = %q", h)
	}
}

// The ubuntu kernel args carry "ds=nocloud-net;s=…" — an unescaped semicolon
// is GRUB's command separator and truncates the linux line there (2288H:
// the kernel booted with no ip=/BOOTIF/nfsroot at all). sanitizeArgs must
// escape it; whitespace collapsing stays.
func TestSanitizeArgsEscapesSemicolon(t *testing.T) {
	in := "autoinstall ds=nocloud-net;s=http://10.0.0.1:8080/render/toku/ ip=198.51.100.180::198.51.100.248:255.255.255.0:::off BOOTIF=01-02-00-00-00-00-00"
	want := `autoinstall ds=nocloud-net\;s=http://10.0.0.1:8080/render/toku/ ip=198.51.100.180::198.51.100.248:255.255.255.0:::off BOOTIF=01-02-00-00-00-00-00`
	if got := sanitizeArgs(in); got != want {
		t.Fatalf("sanitizeArgs:\n got %q\nwant %q", got, want)
	}
	// The rendered grub.cfg keeps the whole line intact after unescaping.
	cfg := RenderGRUB(&Entry{MAC: "02:00:00:00:00:00", TaskID: "tsk", Token: "toku",
		Kernel: "vmlinuz", Initrd: "initrd.img", KernelArgs: in}, "http://10.0.0.1:8080")
	if !strings.Contains(cfg, `\;s=`) {
		t.Fatalf("grub.cfg lost the escaped separator:\n%s", cfg)
	}
	if strings.Contains(cfg, "nocloud-net;s=") {
		t.Fatalf("grub.cfg leaked an unescaped separator (args after it would be dropped):\n%s", cfg)
	}
	if strings.Contains(cfg, "  ") {
		t.Fatalf("grub.cfg has double spaces:\n%s", cfg)
	}
}
