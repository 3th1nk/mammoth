package netboot

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/3th1nk/mammoth/assets/pxe"
)

// The escape hatch kit is the deployment's TFTP root: every file the site
// dnsmasq references must exist, and the trampolines must self-identify the
// client to the existing HTTP endpoints.
func TestExportExternalKit(t *testing.T) {
	dir := t.TempDir()
	if err := ExportExternalKit(dir, pxe.Files, "http://10.0.0.1:8080"); err != nil {
		t.Fatalf("ExportExternalKit: %v", err)
	}

	for _, name := range []string{
		"undionly.kpxe", "shimx64.efi", "grubx64.efi", "shimaa64.efi", "grubaa64.efi",
		"boot.ipxe", "grub/grub.cfg", "dnsmasq.conf.example",
		"grub/x86_64-efi/command.lst", "grub/x86_64-efi/fs.lst",
		"grub/arm64-efi/command.lst", "grub/arm64-efi/fs.lst",
	} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.IsDir() {
			t.Errorf("kit file %s missing: %v", name, err)
		}
	}

	ipxe, _ := os.ReadFile(filepath.Join(dir, "boot.ipxe"))
	if !strings.Contains(string(ipxe), "chain --replace http://10.0.0.1:8080/netboot/script?mac=${net0/mac}") {
		t.Errorf("ipxe trampoline must self-identify via net0/mac:\n%s", ipxe)
	}
	grub, _ := os.ReadFile(filepath.Join(dir, "grub", "grub.cfg"))
	if !strings.Contains(string(grub), "configfile (http,10.0.0.1:8080)/netboot/grub/${net_default_mac}") {
		t.Errorf("grub trampoline must use the (http,host) device + net_default_mac:\n%s", grub)
	}
	dnsmasq, _ := os.ReadFile(filepath.Join(dir, "dnsmasq.conf.example"))
	for _, want := range []string{
		"dhcp-match=set:ipxe,175",
		"dhcp-match=set:efi-aarch64,option:client-arch,11",
		"dhcp-boot=tag:efi64,tag:!ipxe,shimx64.efi",
		"dhcp-boot=tag:efi-aarch64,tag:!ipxe,shimaa64.efi",
		"dhcp-boot=tag:ipxe,boot.ipxe",
		"dhcp-boot=tag:!ipxe,tag:!efi64,tag:!efi-aarch64,undionly.kpxe",
	} {
		if !strings.Contains(string(dnsmasq), want) {
			t.Errorf("dnsmasq example missing %q", want)
		}
	}
}

func TestExportExternalKitRequiresBaseURL(t *testing.T) {
	if err := ExportExternalKit(t.TempDir(), pxe.Files, ""); err == nil {
		t.Fatalf("empty baseURL must be rejected")
	}
}

// ExternalOnly starts nothing on the UDP side — the whole point of the
// escape hatch is deploying beside a site DHCP/TFTP that owns those ports.
func TestStartExternalOnlyBindsNoUDP(t *testing.T) {
	// Occupy 67/69 the way a site server would; external mode must not care.
	blocker, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 6969})
	if err != nil {
		t.Skipf("cannot bind test port: %v", err)
	}
	defer blocker.Close()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, Options{
		ExternalOnly: true,
		BaseURL:      "http://10.0.0.1:8080",
		// Ports deliberately unset → defaults; binding them would fail in
		// restricted environments, and ExternalOnly must not even try.
	})
	if err != nil {
		t.Fatalf("external-only start: %v", err)
	}
	shutdown := cancel
	shutdown()
	if err := s.Wait(); err != nil {
		t.Fatalf("clean shutdown expected, got %v", err)
	}
}

// The trampoline land on the same renderers builtin uses — pin the HTTP
// contract the kit's static files imply.
func TestTrampolineContractRenderers(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := &Entry{MAC: "52:54:00:12:34:56", TaskID: "t1", Kind: "install",
		Token: "tok", Kernel: "vmlinuz", Initrd: "initrd.img", KernelArgs: "ip=dhcp"}
	script := RenderScript(e, srv.URL)
	if !strings.Contains(script, srv.URL+"/netboot/files/tok/vmlinuz ip=dhcp") {
		t.Fatalf("script render changed: %s", script)
	}
	grub := RenderGRUB(e, srv.URL)
	if !strings.Contains(grub, "linux (http,"+strings.TrimPrefix(srv.URL, "http://")+")/netboot/files/tok/vmlinuz") {
		t.Fatalf("grub render changed: %s", grub)
	}
}
