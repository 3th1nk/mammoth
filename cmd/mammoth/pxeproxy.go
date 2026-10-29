package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"path/filepath"
	"strings"
	"sync"
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
	// Container-friendly: every flag has an env override
	// (MAMMOTH_PXEPROXY_*), so compose files pass configuration without
	// editing the command line.
	envOr := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	envInt := func(key string, def int) int {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return def
	}
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
	pf.StringVar(&o.NextServer, "next-server", envOr("MAMMOTH_PXEPROXY_NEXT_SERVER", ""), "this host's IP on the provisioning segment (siaddr/TFTP-server offered to PXE clients)")
	pf.StringVar(&o.BaseURL, "base-url", envOr("MAMMOTH_PXEPROXY_BASE_URL", ""), "mammoth engine machine-face base URL (e.g. http://172.16.20.250:8091)")
	pf.StringVar(&o.TFTPRoot, "tftp-root", envOr("MAMMOTH_PXEPROXY_TFTP_ROOT", "/var/lib/mammoth/tftp"), "directory holding the exported external kit (auto-synced from the engine; created on first sync)")
	syncDef := envOr("MAMMOTH_PXEPROXY_SYNC_INTERVAL", "5m")
	syncDur, syncErr := time.ParseDuration(syncDef)
	if syncErr != nil {
		cmd.Printf("pxe-proxy: bad MAMMOTH_PXEPROXY_SYNC_INTERVAL %q (%v) — using default 5m\n", syncDef, syncErr)
		syncDur = 5 * 60 * time.Second
	}
	pf.DurationVar(&o.SyncInterval, "sync-interval", syncDur, "kit re-sync period; 0 = sync once at startup")
	pf.StringVar(&o.KitToken, "kit-token", envOr("MAMMOTH_PXEPROXY_KIT_TOKEN", ""), "bearer token for the engine kit endpoint (empty = anonymous; /netboot routes are public by default)")
	pf.StringVar(&o.Segment, "segment", envOr("MAMMOTH_PXEPROXY_SEGMENT", ""), "provisioning segment label reported with sightings (informational)")
	pf.IntVar(&o.DHCPPort, "dhcp-port", envInt("MAMMOTH_PXEPROXY_DHCP_PORT", 67), "proxyDHCP listen port (coexists with the site DHCP across the L2)")
	pf.IntVar(&o.ProxyPort, "proxy-port", envInt("MAMMOTH_PXEPROXY_PROXY_PORT", 4011), "PXE boot-server discovery port")
	pf.IntVar(&o.TFTPPort, "tftp-port", envInt("MAMMOTH_PXEPROXY_TFTP_PORT", 69), "TFTP port")
	pf.IntVar(&o.SyslogPort, "syslog-port", envInt("MAMMOTH_PXEPROXY_SYSLOG_PORT", 514), "installer-log sink port (bind failure degrades to a warning)")
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
	Segment      string
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
	etagFile := o.TFTPRoot + ".etag"
	known, _ := os.ReadFile(etagFile)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if o.KitToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.KitToken)
	}
	if len(known) > 0 {
		req.Header.Set("If-None-Match", strings.TrimSpace(string(known)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("kit fetch: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil // unchanged since the last sync — nothing to do
	case http.StatusOK:
	default:
		return fmt.Errorf("kit fetch: HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(o.TFTPRoot), 0o755); err != nil {
		return err
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
	// Two-step swap: the old root moves aside before the new one lands —
	// the window where TFTP sees no root shrinks from "across the whole
	// RemoveAll" to "between two renames" (microseconds), and any request
	// in that window is a client retry, never a stranded boot.
	previous := filepath.Join(filepath.Dir(tmp), "kit-previous")
	_ = os.RemoveAll(previous)
	if err := os.Rename(o.TFTPRoot, previous); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, o.TFTPRoot); err != nil {
		// Put the old root back — a failed swap must not strand machines.
		_ = os.Rename(previous, o.TFTPRoot)
		return err
	}
	_ = os.RemoveAll(previous)
	if etag := resp.Header.Get("ETag"); etag != "" {
		_ = os.WriteFile(etagFile, []byte(strings.TrimSpace(etag)), 0o644)
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
	// Sightings feed: every PXE DISCOVER the agent answers is reported to
	// the engine's observation endpoint (external deployments are blind to
	// the wire — the agent is the only eye). Async fire-and-forget with a
	// per-client dedupe window: PXE ROMs retry DISCOVERs aggressively and
	// the endpoint is a courtesy feed, not a log.
	obs := &sightingFeed{base: base, token: o.KitToken, segment: o.Segment, seen: map[string]time.Time{}}
	return netboot.Options{
		DHCPPort:   o.DHCPPort,
		ProxyPort:  o.ProxyPort,
		TFTPPort:   o.TFTPPort,
		SyslogPort: o.SyslogPort,
		NextServer: ns,
		BaseURL:    base,
		NBPs:       os.DirFS(root),
		OnObserve: func(mac string, arch netboot.Arch) {
			obs.report(mac, string(arch))
		},
	}, nil
}

// sightingFeed reports PXE sightings to the engine's observation endpoint,
// deduplicated per client+arch within a window (PXE ROMs retry DISCOVERs
// aggressively; the engine's TouchByMAC is an upsert — the dedupe keeps the
// wire and the DB write at one sighting per client per window).
type sightingFeed struct {
	base    string
	token   string
	segment string
	mu      sync.Mutex
	seen    map[string]time.Time
}

const sightingWindow = 10 * time.Minute

func (f *sightingFeed) report(mac, arch string) {
	f.mu.Lock()
	key := mac + "/" + arch
	if t, ok := f.seen[key]; ok && time.Since(t) < sightingWindow {
		f.mu.Unlock()
		return
	}
	f.seen[key] = time.Now()
	f.mu.Unlock()
	go func() {
		body, _ := json.Marshal(map[string]string{"mac": mac, "firmware": arch, "segment": f.segment})
		req, err := http.NewRequest(http.MethodPost, f.base+"/netboot/observe", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if f.token != "" {
			req.Header.Set("Authorization", "Bearer "+f.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}
