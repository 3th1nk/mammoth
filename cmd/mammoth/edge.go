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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/3th1nk/mammoth/internal/config"
	"github.com/3th1nk/mammoth/internal/netboot"
)

// edgeCobraCommand wraps the provisioning-VLAN edge service as a cobra
// subcommand: proxyDHCP answers (67/4011) + TFTP (69) serving the exported
// external kit, kit auto-sync from the engine, the Wake-on-LAN relay
// (--wol-listen) and PXE sighting feedback (observe) — the engine's
// per-segment deputy on any host in the machine VLAN (renamed from
// pxe-proxy: the duties outgrew the boot-server proxy the name described).
// All install decisions stay with the mammoth engine (the kit's boot.ipxe
// chains to the engine's per-MAC script over HTTP) — one binary per
// provisioning VLAN replaces a hand-tuned dnsmasq for cross-subnet
// deployments.
func edgeCobraCommand() *cobra.Command {
	// Container-friendly: every flag has an env override
	// (MAMMOTH_EDGE_*; 旧 MAMMOTH_PXEPROXY_* 仍兼容读取), so compose files pass configuration without
	// editing the command line.
	// legacyEnv maps a MAMMOTH_EDGE_* key to its pre-rename
	// MAMMOTH_PXEPROXY_* twin: existing deployments (edge.env, systemd
	// units) keep working across the rename until their assets migrate.
	legacyEnv := func(key string) string {
		if rest, ok := strings.CutPrefix(key, "MAMMOTH_EDGE_"); ok {
			return "MAMMOTH_PXEPROXY_" + rest
		}
		return ""
	}
	envOr := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if legacy := legacyEnv(key); legacy != "" {
			if v := os.Getenv(legacy); v != "" {
				return v
			}
		}
		return def
	}
	envInt := func(key string, def int) int {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		if legacy := legacyEnv(key); legacy != "" {
			if v := os.Getenv(legacy); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
		return def
	}
	var o edgeOptions
	cmd := &cobra.Command{
		Use:     "edge",
		Aliases: []string{"pxe-proxy"}, // 旧名别名:现役部署资产迁移期保留
		Short:   "Run the provisioning-VLAN edge service (proxyDHCP + TFTP + kit sync + WoL relay + sightings)",
		Long: `Run the provisioning-VLAN PXE first hop on any host in the machine
segment: proxyDHCP answers (67/4011) hand out the boot chain, TFTP (69)
serves the exported external kit (NBP binaries + trampoline). All install
decisions stay with the mammoth engine — the kit's boot.ipxe chains to the
engine's per-MAC script over HTTP. One binary per provisioning VLAN
replaces a hand-tuned dnsmasq; the kit is exported by the engine
(MediaDir/netboot/external-tftp) and copied here.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.CalledAs() != "edge" {
				cmd.Printf("edge: %q is the deprecated alias of this command (mammoth edge) — update the unit/compose before the next release\n", cmd.CalledAs())
			}
			return runEdge(o)
		},
	}
	pf := cmd.Flags()
	pf.StringVar(&o.NextServer, "next-server", envOr("MAMMOTH_EDGE_NEXT_SERVER", ""), "this host's IP on the provisioning segment (siaddr/TFTP-server offered to PXE clients)")
	pf.StringVar(&o.BaseURL, "base-url", envOr("MAMMOTH_EDGE_BASE_URL", ""), "mammoth engine machine-face base URL (e.g. http://172.16.20.250:8080)")
	pf.StringVar(&o.TFTPRoot, "tftp-root", envOr("MAMMOTH_EDGE_TFTP_ROOT", "/var/lib/mammoth/tftp"), "directory holding the exported external kit (auto-synced from the engine; created on first sync)")
	syncDef := envOr("MAMMOTH_EDGE_SYNC_INTERVAL", "5m")
	syncDur, syncErr := time.ParseDuration(syncDef)
	if syncErr != nil {
		cmd.Printf("edge: bad MAMMOTH_EDGE_SYNC_INTERVAL %q (%v) — using default 5m\n", syncDef, syncErr)
		syncDur = 5 * 60 * time.Second
	}
	pf.DurationVar(&o.SyncInterval, "sync-interval", syncDur, "kit re-sync period; 0 = sync once at startup")
	pf.StringVar(&o.KitToken, "kit-token", envOr("MAMMOTH_EDGE_KIT_TOKEN", ""), "bearer token for the engine kit endpoint (empty = anonymous; /netboot routes are public by default)")
	pf.StringVar(&o.Segment, "segment", envOr("MAMMOTH_EDGE_SEGMENT", ""), "provisioning segment label reported with sightings (informational)")
	pf.StringVar(&o.UEFINBP, "uefi-nbp", envOr("MAMMOTH_EDGE_UEFI_NBP", "shim"), `UEFI NBP chain: shim (default, Secure Boot holds) | ipxe (plain unsigned iPXE — grub2 cannot consume Proxy DHCP messages in UEFI mode (RackN KB-00043, RH #1809246), so behind an unmodifiable site DHCP the chain must switch; requires Secure Boot off)`)
	pf.IntVar(&o.DHCPPort, "dhcp-port", envInt("MAMMOTH_EDGE_DHCP_PORT", 67), "proxyDHCP listen port (coexists with the site DHCP across the L2)")
	pf.IntVar(&o.ProxyPort, "proxy-port", envInt("MAMMOTH_EDGE_PROXY_PORT", 4011), "PXE boot-server discovery port")
	pf.IntVar(&o.TFTPPort, "tftp-port", envInt("MAMMOTH_EDGE_TFTP_PORT", 69), "TFTP port")
	pf.IntVar(&o.SyslogPort, "syslog-port", envInt("MAMMOTH_EDGE_SYSLOG_PORT", 514), "installer-log sink port (bind failure degrades to a warning)")
	pf.StringVar(&o.WoLListen, "wol-listen", envOr("MAMMOTH_EDGE_WOL_LISTEN", ""), `HTTP listen address for the Wake-on-LAN relay (e.g. ":9100", empty = off) — the engine calls POST /wol {"mac": ...} for cross-segment headless boots: WoL is an L2 broadcast and the engine is not on the machine's segment, this proxy is`)
	pf.StringVar(&o.WoLToken, "wol-token", envOr("MAMMOTH_EDGE_WOL_TOKEN", ""), "bearer token required on /wol requests (empty = unauthenticated; the provisioning L2 is the trust boundary, same as the kit)")
	pf.StringVar(&o.WoLBroadcast, "wol-broadcast", envOr("MAMMOTH_EDGE_WOL_BROADCAST", "255.255.255.255"), "broadcast address the relay sends magic packets to (the provisioning segment's directed broadcast is the precise form)")
	_ = cmd.MarkFlagRequired("next-server")
	_ = cmd.MarkFlagRequired("base-url")
	return cmd
}

type edgeOptions struct {
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
	UEFINBP      string
	WoLListen    string
	WoLToken     string
	WoLBroadcast string
}

// runEdge validates the flags, syncs the kit, and serves until the
// context is cancelled.
func runEdge(o edgeOptions) error {
	opts, err := o.netbootOptions()
	if err != nil {
		return err
	}
	// Kit sync before the first bind: a stale or missing TFTP root strands
	// machines at the PXE prompt exactly like a failed bind would.
	if err := syncKit(o); err != nil {
		return fmt.Errorf("edge: initial kit sync: %w", err)
	}
	if o.SyncInterval > 0 {
		go func() {
			t := time.NewTicker(o.SyncInterval)
			defer t.Stop()
			for range t.C {
				if err := syncKit(o); err != nil {
					fmt.Fprintf(os.Stderr, "edge: kit re-sync: %v\n", err)
				}
			}
		}()
	}
	srv, err := netboot.Start(mustContext(), opts)
	if err != nil {
		return err
	}
	if o.WoLListen != "" {
		go serveWoLRelay(o)
	}
	return srv.Wait()
}

// serveWoLRelay runs the Wake-on-LAN relay's tiny HTTP face: one endpoint,
// POST /wol {"mac": "..."} → the magic packet on this segment's broadcast.
// The relay exists because WoL is an L2 broadcast and the engine — usually
// parked on an ops segment — cannot reach the machine segment's sleepers;
// the proxy IS on the machine segment by construction. A bind failure is
// fatal for the relay goroutine but must not kill the PXE service (the
// manual power-on fallback keeps headless boots alive without it).
func serveWoLRelay(o edgeOptions) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wol", func(w http.ResponseWriter, r *http.Request) {
		if o.WoLToken != "" && r.Header.Get("Authorization") != "Bearer "+o.WoLToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		mac, err := netboot.ParseWoLRequest(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := netboot.SendWoL(o.WoLBroadcast, mac); err != nil {
			fmt.Fprintf(os.Stderr, "edge: wol relay %s: %v\n", mac, err)
			http.Error(w, "send failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := http.ListenAndServe(o.WoLListen, mux); err != nil {
		fmt.Fprintf(os.Stderr, "edge: wol relay on %s failed: %v (manual power-on remains the fallback)\n", o.WoLListen, err)
	}
}

// wakeOnLANClient bounds the relay call — a hung proxy must not stall the
// boot stage's wake step (the evidence wait that follows is the real gate).
var wakeOnLANClient = &http.Client{Timeout: 10 * time.Second}

// wakeOnLANFunc composes the engine's headless wake path (docs/
// 13-headless-design.md §4): the direct L2 broadcast when the engine sits
// on the machine segment (MAMMOTH_PXE_WOL_BROADCAST), then the segment
// proxy's relay for the cross-segment shape (MAMMOTH_PXE_WOL_PROXY_URL —
// the proxy runs with --wol-listen). nil = neither configured; the manual
// power-on fallback covers the deployment.
func wakeOnLANFunc(cfg config.Config) func(mac string) error {
	direct, proxy := cfg.PXEWoLBroadcast != "", cfg.PXEWoLProxyURL != ""
	if !direct && !proxy {
		return nil
	}
	return func(mac string) error {
		var firstErr error
		if direct {
			if err := netboot.SendWoL(cfg.PXEWoLBroadcast, mac); err != nil {
				firstErr = err
			} else {
				return nil
			}
		}
		if proxy {
			body, _ := json.Marshal(netboot.WoLRequest{MAC: mac})
			req, err := http.NewRequest(http.MethodPost,
				strings.TrimSuffix(cfg.PXEWoLProxyURL, "/")+"/wol", bytes.NewReader(body))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				if cfg.PXEWoLProxyToken != "" {
					req.Header.Set("Authorization", "Bearer "+cfg.PXEWoLProxyToken)
				}
				resp, derr := wakeOnLANClient.Do(req)
				if derr == nil {
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusNoContent {
						return nil
					}
					err = fmt.Errorf("wol relay answered %s", resp.Status)
				} else {
					err = derr
				}
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
}

// syncKit pulls the engine's exported external kit
// (<base-url>/netboot/external-kit.tar) and atomically replaces the TFTP
// root: download to a temp directory, then swap. A failed sync keeps the
// previous root intact — a broken download must not strand machines at the
// PXE prompt.
func syncKit(o edgeOptions) error {
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

func (o edgeOptions) netbootOptions() (netboot.Options, error) {
	ns := net.ParseIP(strings.TrimSpace(o.NextServer))
	if ns == nil {
		return netboot.Options{}, fmt.Errorf("edge: --next-server %q is not an IPv4 address", o.NextServer)
	}
	base := strings.TrimSuffix(strings.TrimSpace(o.BaseURL), "/")
	if base == "" {
		return netboot.Options{}, fmt.Errorf("edge: --base-url is required")
	}
	switch o.UEFINBP {
	case "", "shim":
	case "ipxe":
	default:
		return netboot.Options{}, fmt.Errorf("edge: --uefi-nbp %q is not shim|ipxe", o.UEFINBP)
	}
	root := strings.TrimSpace(o.TFTPRoot)
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return netboot.Options{}, fmt.Errorf("edge: --tftp-root %q is not a directory (export the engine kit: MediaDir/netboot/external-tftp)", root)
	}
	// Sightings feed: every PXE DISCOVER the agent answers is reported to
	// the engine's observation endpoint (external deployments are blind to
	// the wire — the agent is the only eye). Async fire-and-forget with a
	// per-client dedupe window: PXE ROMs retry DISCOVERs aggressively and
	// the endpoint is a courtesy feed, not a log.
	obs := &sightingFeed{base: base, token: o.KitToken, segment: o.Segment, seen: map[string]time.Time{}}
	return netboot.Options{
		DHCPPort:     o.DHCPPort,
		ProxyPort:    o.ProxyPort,
		TFTPPort:     o.TFTPPort,
		SyslogPort:   o.SyslogPort,
		NextServer:   ns,
		BaseURL:      base,
		NBPs:         os.DirFS(root),
		UEFIPlainNBP: o.UEFINBP == "ipxe",
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
