package netboot

import (
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The external-PXE escape hatch kit (docs/operations.md §pxe-external): the
// static file set a site DHCP+TFTP server (dnsmasq) needs so mammoth never
// has to own UDP 67/69. The chain works by CLIENT SELF-IDENTIFICATION — the
// only dynamic decisions mammoth makes are HTTP fetches where the client
// names itself:
//
//	BIOS      DHCP → TFTP undionly.kpxe → iPXE re-DHCP (tagged) → TFTP
//	          boot.ipxe (static) → chain .../netboot/script?mac=${net0/mac}
//	UEFI x64  DHCP → TFTP shimx64.efi → grubx64.efi → TFTP grub/grub.cfg
//	          (static) → configfile (http,mammoth)/netboot/grub/${net_default_mac}
//	UEFI aa64 DHCP → TFTP shimaa64.efi → grubaa64.efi → same grub trampoline
//	          — both shim/grub pairs stay the Debian-signed sets, so Secure
//	          Boot holds on either architecture.
//
// Everything after the trampolines is the builtin machinery: the script
// endpoint resolves the MAC against the entry registry (with the enrollment
// fallback), the grub endpoint renders the same per-MAC config the builtin
// TFTP render hook does.

// ExportExternalKit materializes the TFTP root for the site server: the NBP
// binaries and grub module tables from the embedded assets (nbps), plus the
// two trampolines and a ready-to-edit dnsmasq.conf.example, all under dir.
// baseURL is the machine-face URL the trampolines point at.
func ExportExternalKit(dir string, nbps fs.FS, baseURL string) error {
	if baseURL == "" {
		return fmt.Errorf("netboot: external kit needs the machine-face base URL")
	}
	host := grubHTTPHost(baseURL)
	// iPXE trampolines want a full URL; grub wants the (http,host:port)
	// device — same source, two renderings.
	ipxeBase := strings.TrimSuffix(baseURL, "/")

	files := map[string][]byte{
		"undionly.kpxe":        nil,
		"shimx64.efi":          nil,
		"grubx64.efi":          nil,
		"shimaa64.efi":         nil,
		"grubaa64.efi":         nil,
		"ipxe-amd64.efi":       nil, // the wimboot carrier's host (Windows; Secure Boot off)
		"boot.ipxe":            []byte(externalIPXETrampoline(ipxeBase)),
		"grub/grub.cfg":        []byte(externalGRUBTrampoline(host)),
		"dnsmasq.conf.example": []byte(ExternalDnsmasqExample(host, ipxeBase)),
	}
	if err := os.MkdirAll(filepath.Join(dir, "grub"), 0o755); err != nil {
		return err
	}
	for name, content := range files {
		if content == nil {
			// NBP binary: copy verbatim out of the embedded assets.
			if err := copyFSFile(nbps, name, filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("netboot: external kit asset %s: %w", name, err)
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			return err
		}
	}
	// grubnet's module tables under its (tftp)/grub/ prefix — the whole
	// subtree, same layout the builtin TFTP serves, for both architectures
	// (x64 and aarch64 grubnet fetch the same (tftp)/grub/ paths).
	for _, arch := range []string{"x86_64-efi", "arm64-efi"} {
		if err := fs.WalkDir(nbps, "grub/"+arch, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			dest := filepath.Join(dir, filepath.FromSlash(path))
			if d.IsDir() {
				return os.MkdirAll(dest, 0o755)
			}
			return copyFSFile(nbps, path, dest)
		}); err != nil {
			return err
		}
	}
	return nil
}

func copyFSFile(fsys fs.FS, name, dest string) error {
	src, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// externalIPXETrampoline is the static boot.ipxe the external TFTP serves to
// tagged iPXE clients: iPXE expands ${net0/mac}, so the script endpoint's
// existing per-MAC machinery applies unchanged.
func externalIPXETrampoline(baseURL string) string {
	return fmt.Sprintf(`#!ipxe
# mammoth external-PXE trampoline (docs/operations.md §pxe-external).
# iPXE self-identifies: ${net0/mac} expands to the booting NIC's address.
# Multi-NIC caveat: net0 is iPXE's first detected interface — on hosts where
# the PXE ROM is not net0 the lookup misses and the machine falls through to
# disk (the escape hatch trades that edge for zero UDP ownership).
chain --replace %s/netboot/script?mac=${net0/mac}
`, baseURL)
}

// externalGRUBTrampoline is the static grub/grub.cfg the external TFTP
// serves into grubnet's prefix: grub expands ${net_default_mac} in the
// configfile path, landing the fetch on the HTTP per-MAC config endpoint.
// The (http,host:port) device syntax is load-bearing — grub treats bare
// http:// URLs as TFTP filenames (builtin-mode finding).
func externalGRUBTrampoline(host string) string {
	return fmt.Sprintf(`# mammoth external-PXE trampoline (docs/operations.md §pxe-external).
# grubnet brought its network up to fetch this file; ${net_default_mac}
# expands to the booting NIC's address for the HTTP config fetch.
configfile (http,%s)/netboot/grub/${net_default_mac}
`, host)
}

// ExternalDnsmasqExample renders a ready-to-edit site dnsmasq config: real
// DHCP (the escape hatch's premise is that the site owns addressing), TFTP
// for the static kit, and arch/iPXE tags that route each firmware class to
// its chain. <tftp-server> placeholders are the operator's TFTP address; the
// HTTP hosts are already filled from the deployment's base URL.
func ExternalDnsmasqExample(grubHost, ipxeBase string) string {
	u, err := url.Parse(ipxeBase)
	mammoth := ipxeBase
	if err == nil && u.Host != "" {
		mammoth = u.Host
	}
	return fmt.Sprintf(`# mammoth external-PXE escape hatch — site dnsmasq config (docs/operations.md §pxe-external).
# Edit the subnet/router/tftp-server placeholders for your site, keep the
# tag routing. The TFTP root is the directory netboot's external kit was
# exported to (MediaDir/netboot/external-tftp).
#
dhcp-authoritative
dhcp-range=192.168.77.50,192.168.77.150,255.255.255.0,12h
dhcp-option=option:router,192.168.77.1
enable-tftp
tftp-root=/srv/mammoth-external-tftp

# Client classes: iPXE announces option 175; UEFI x64 arrives with client
# arch 7 (EFI BC) or 9 (EFI x86-64); UEFI aarch64 with arch 11.
dhcp-match=set:ipxe,175
dhcp-match=set:efi64,option:client-arch,7
dhcp-match=set:efi64,option:client-arch,9
dhcp-match=set:efi-aarch64,option:client-arch,11

# BIOS, first boot: PXE ROM gets the undiom layer that turns it into iPXE.
dhcp-boot=tag:!ipxe,tag:!efi64,tag:!efi-aarch64,undionly.kpxe,,<tftp-server>
# UEFI x64, Secure Boot chain: Microsoft-signed shim loads Debian-signed grubnet.
dhcp-boot=tag:efi64,tag:!ipxe,shimx64.efi,,<tftp-server>
# UEFI aarch64, Secure Boot chain: same shape, the aa64 signed pair.
dhcp-boot=tag:efi-aarch64,tag:!ipxe,shimaa64.efi,,<tftp-server>
# iPXE (all chains, second boot): the static trampoline self-identifies
# with ${net0/mac} and chains to mammoth's per-MAC script over HTTP.
dhcp-boot=tag:ipxe,boot.ipxe,,<tftp-server>

# mammoth machine face (HTTP): %s — no UDP service on it in this mode.
# The grub chain fetches its per-MAC config from (http,%s)/netboot/grub/<mac>.
#
# Windows machines (the wimboot carrier) boot through plain iPXE instead of
# the Secure Boot chain — wimboot has no shim/grub path and is unsigned. Pin
# each Windows machine's MAC to the iPXE binary (Secure Boot OFF required):
#   dhcp-host=<win-machine-mac>,set:winboot
#   dhcp-boot=tag:winboot,tag:!ipxe,ipxe-amd64.efi,,<tftp-server>
# The second DHCP round is already iPXE (option 175) and takes the shared
# boot.ipxe trampoline above.
`, mammoth, grubHost)
}
