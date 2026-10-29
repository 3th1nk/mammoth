package main

import (
	"archive/tar"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/3th1nk/mammoth/internal/netboot"
)

// pxeProxyCobraCommand wraps the provisioning-VLAN first hop as a cobra
// subcommand: proxyDHCP answers (67/4011) + TFTP (69) serving the exported
// external kit, on any host in the machine VLAN. All install decisions stay
// with the mammoth engine (the kit's boot.ipxe chains to the engine's
// per-MAC script over HTTP) — one binary per provisioning VLAN replaces a
// hand-tuned dnsmasq for cross-subnet deployments.
func pxeProxyCobraCommand() *cobra.Command {
	var o pxeProxyOptions
	cmd := &cobra.Command{
		Use:   "pxe-proxy",
		Short: "Run the provisioning-VLAN PXE first hop (proxyDHCP + TFTP)",
		Long: `Run the provisioning-VLAN PXE first hop on any host in the machine
segment: proxyDHCP answers (67/4011) hand out the boot chain, TFTP (69)
serves the exported external kit (NBP binaries + trampoline). All install
decisions stay with the mammoth engine — the kit's boot.ipxe chains to the
engine's per-MAC script over HTTP. One binary per provisioning VLAN
replaces a hand-tuned dnsmasq; the kit is exported by the engine
(MediaDir/netboot/external-tftp) and copied here.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPxeProxy(o)
		},
	}
	pf := cmd.Flags()
	pf.StringVar(&o.NextServer, "next-server", "", "this host's IP on the provisioning segment (siaddr/TFTP-server offered to PXE clients)")
	pf.StringVar(&o.BaseURL, "base-url", "", "mammoth engine machine-face base URL (e.g. http://172.16.20.250:8091)")
	pf.StringVar(&o.TFTPRoot, "tftp-root", "/srv/mammoth-external-tftp", "directory holding the exported external kit (auto-synced from the engine)")
	pf.DurationVar(&o.SyncInterval, "sync-interval", 5*60*time.Second, "kit re-sync period; 0 = sync once at startup")
	pf.StringVar(&o.KitToken, "kit-token", "", "bearer token for the engine kit endpoint (empty = anonymous; /netboot routes are public by default)")
	pf.IntVar(&o.DHCPPort, "dhcp-port", 67, "proxyDHCP listen port (coexists with the site DHCP across the L2)")
	pf.IntVar(&o.ProxyPort, "proxy-port", 4011, "PXE boot-server discovery port")
	pf.IntVar(&o.TFTPPort, "tftp-port", 69, "TFTP port")
	pf.IntVar(&o.SyslogPort, "syslog-port", 514, "installer-log sink port (bind failure degrades to a warning)")
	_ = cmd.MarkFlagRequired("next-server")
	_ = cmd.MarkFlagRequired("base-url")
	return cmd
}

type pxeProxyOptions struct {
	NextServer   string
	BaseURL      string
	TFTPRoot     string
	DHCPPort     int
	ProxyPort    int
	TFTPPort     int
	SyslogPort   int
	SyncInterval time.Duration
	KitToken     string
}

// runPxeProxy validates the flags, syncs the kit, and serves until the
// context is cancelled.
func runPxeProxy(o pxeProxyOptions) error {
	opts, err := o.netbootOptions()
	if err != nil {
		return err
	}
	// Kit sync before the first bind: a stale or missing TFTP root strands
	// machines at the PXE prompt exactly like a failed bind would.
	if err := syncKit(o); err != nil {
		return fmt.Errorf("pxe-proxy: initial kit sync: %w", err)
	}
	if o.SyncInterval > 0 {
		go func() {
			t := time.NewTicker(o.SyncInterval)
			defer t.Stop()
			for range t.C {
				if err := syncKit(o); err != nil {
					fmt.Fprintf(os.Stderr, "pxe-proxy: kit re-sync: %v\n", err)
				}
			}
		}()
	}
	srv, err := netboot.Start(mustContext(), opts)
	if err != nil {
		return err
	}
	return srv.Wait()
}

// syncKit pulls the engine's exported external kit
// (<base-url>/netboot/external-kit.tar) and atomically replaces the TFTP
// root: download to a temp directory, then swap. A failed sync keeps the
// previous root intact — a broken download must not strand machines at the
// PXE prompt.
func syncKit(o pxeProxyOptions) error {
	url := strings.TrimSuffix(o.BaseURL, "/") + "/netboot/external-kit.tar"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if o.KitToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.KitToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("kit fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kit fetch: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(o.TFTPRoot), "kit-sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("kit tar: %w", err)
		}
		name := filepath.Clean(filepath.Join("/", hdr.Name))
		if strings.HasPrefix(name, "..") {
			continue
		}
		target := filepath.Join(tmp, name)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	// Atomic-ish swap: move the new tree into place. The TFTP server reads
	// through its own root handle, so the rename window is a dropped request
	// at worst — the next client retry re-opens the new root.
	if err := os.RemoveAll(o.TFTPRoot); err != nil {
		return err
	}
	if err := os.Rename(tmp, o.TFTPRoot); err != nil {
		return err
	}
	return nil
}

func (o pxeProxyOptions) netbootOptions() (netboot.Options, error) {
	ns := net.ParseIP(strings.TrimSpace(o.NextServer))
	if ns == nil {
		return netboot.Options{}, fmt.Errorf("pxe-proxy: --next-server %q is not an IPv4 address", o.NextServer)
	}
	base := strings.TrimSuffix(strings.TrimSpace(o.BaseURL), "/")
	if base == "" {
		return netboot.Options{}, fmt.Errorf("pxe-proxy: --base-url is required")
	}
	root := strings.TrimSpace(o.TFTPRoot)
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return netboot.Options{}, fmt.Errorf("pxe-proxy: --tftp-root %q is not a directory (export the engine kit: MediaDir/netboot/external-tftp)", root)
	}
	return netboot.Options{
		DHCPPort:   o.DHCPPort,
		ProxyPort:  o.ProxyPort,
		TFTPPort:   o.TFTPPort,
		SyslogPort: o.SyslogPort,
		NextServer: ns,
		BaseURL:    base,
		NBPs:       os.DirFS(root),
	}, nil
}
