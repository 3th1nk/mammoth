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

func TestFileNamesAllowlist(t *testing.T) {
	e := &Entry{Kernel: "vmlinuz", Initrd: "initrd", Extra: map[string]string{"modloop": "modloop-lts"}}
	names := map[string]bool{}
	for _, n := range e.FileNames() {
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
	c := newCachedResolver(inner, time.Minute)

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
	c := newCachedResolver(inner, 10*time.Millisecond)
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
}
