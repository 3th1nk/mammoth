package main

import (
	"fmt"
	"net"
	"os"
	"strings"

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
	pf.StringVar(&o.TFTPRoot, "tftp-root", "/srv/mammoth-external-tftp", "directory holding the exported external kit (NBP binaries + boot.ipxe)")
	pf.IntVar(&o.DHCPPort, "dhcp-port", 67, "proxyDHCP listen port (coexists with the site DHCP across the L2)")
	pf.IntVar(&o.ProxyPort, "proxy-port", 4011, "PXE boot-server discovery port")
	pf.IntVar(&o.TFTPPort, "tftp-port", 69, "TFTP port")
	pf.IntVar(&o.SyslogPort, "syslog-port", 514, "installer-log sink port (bind failure degrades to a warning)")
	_ = cmd.MarkFlagRequired("next-server")
	_ = cmd.MarkFlagRequired("base-url")
	return cmd
}

type pxeProxyOptions struct {
	NextServer string
	BaseURL    string
	TFTPRoot   string
	DHCPPort   int
	ProxyPort  int
	TFTPPort   int
	SyslogPort int
}

// runPxeProxy validates the flags and serves until the context is cancelled.
func runPxeProxy(o pxeProxyOptions) error {
	opts, err := o.netbootOptions()
	if err != nil {
		return err
	}
	srv, err := netboot.Start(mustContext(), opts)
	if err != nil {
		return err
	}
	return srv.Wait()
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
