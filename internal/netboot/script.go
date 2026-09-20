package netboot

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Entry is one pending network boot registration — the value the HTTP
// script endpoint resolves by MAC and the shape provision writes when a
// task arms its machine for PXE (docs/06-install-pipeline.md, boot strategy
// "pxe"). Kernel/Initrd are bare file names inside the boot tree rooted at
// netboot/files/<Token>/; KernelArgs is interpolated verbatim into the iPXE
// kernel line (modloop= and inst.ks= style URLs are baked in upstream).
type Entry struct {
	MAC        string
	TaskID     string
	Kind       string // install | probe
	Token      string // machine-facing credential + boot tree directory name
	Kernel     string
	Initrd     string
	KernelArgs string
	// Extra lists additional files the kernel args may reference (e.g. the
	// alpine modloop). The HTTP file handler serves exactly these names.
	Extra map[string]string
}

// FileNames returns the allowlist of files servable for this entry.
func (e *Entry) AllowlistedFiles() []string {
	names := make([]string, 0, 2+len(e.Extra))
	for _, n := range []string{e.Kernel, e.Initrd} {
		if n != "" {
			names = append(names, n)
		}
	}
	for _, n := range e.Extra {
		if n != "" {
			names = append(names, n)
		}
	}
	return names
}

// GrantedSubtrees lists subtree grants: Extra values shaped "dir:<name>"
// (e.g. the probe's apk repo) allow everything under that directory.
func (e *Entry) GrantedSubtrees() []string {
	var out []string
	for _, n := range e.Extra {
		if rest, ok := strings.CutPrefix(n, "dir:"); ok && rest != "" {
			out = append(out, rest)
		}
	}
	return out
}

// Resolver answers boot lookups by MAC (normalized lowercase colon form).
// It returns (nil, nil) when no entry is pending — the script endpoint then
// serves the exit fallback. Implementations live behind the store; the
// package stays persistence-free.
type Resolver interface {
	Entry(ctx context.Context, mac string) (*Entry, error)
}

// ResolverFunc adapts a function to Resolver.
type ResolverFunc func(ctx context.Context, mac string) (*Entry, error)

// Entry implements Resolver.
func (f ResolverFunc) Entry(ctx context.Context, mac string) (*Entry, error) {
	return f(ctx, mac)
}

// scriptURLFor builds the per-client iPXE script URL embedded in DHCP
// replies for clients that already run iPXE.
func scriptURLFor(baseURL, mac string, arch Arch) string {
	return fmt.Sprintf("%s/netboot/script?mac=%s&arch=%s", baseURL, mac, arch)
}

// RenderScript renders the iPXE script for an entry. Everything large rides
// HTTP under <baseURL>/netboot/files/<token>/; the kernel args already carry
// absolute URLs (answer file, NFS repo, modloop) composed by provision.
// wimboot entries (the Windows carrier) render the wimboot shape instead:
// the loader is the "kernel" and every boot file is an initrd line with an
// explicit memory name (docs: ipxe.org/wimboot) — the file list is the
// Extra allowlist plus boot.wim, order-free because wimboot fetches by name.
func RenderScript(e *Entry, baseURL string) string {
	if e.Kernel == "wimboot" {
		return renderWimbootScript(e, baseURL)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#!ipxe\n")
	fmt.Fprintf(&b, "# mammoth boot entry mac=%s task=%s kind=%s\n", e.MAC, e.TaskID, e.Kind)
	fmt.Fprintf(&b, "kernel %s/netboot/files/%s/%s %s\n",
		strings.TrimSuffix(baseURL, "/"), e.Token, e.Kernel, sanitizeArgs(e.KernelArgs))
	fmt.Fprintf(&b, "initrd %s/netboot/files/%s/%s\n",
		strings.TrimSuffix(baseURL, "/"), e.Token, e.Initrd)
	b.WriteString("boot\n")
	return b.String()
}

// renderWimbootScript is the Windows carrier script: wimboot takes the
// media's boot files (bootmgr / bootmgfw.efi, BCD, boot.sdi) plus the
// augmented boot.wim and assembles the WinPE memory environment itself —
// there are no kernel args to interpolate.
func renderWimbootScript(e *Entry, baseURL string) string {
	base := fmt.Sprintf("%s/netboot/files/%s/", strings.TrimSuffix(baseURL, "/"), e.Token)
	names := make([]string, 0, len(e.Extra)+1)
	for _, n := range e.Extra {
		if n != "" && !strings.HasPrefix(n, "dir:") {
			names = append(names, n)
		}
	}
	names = append(names, e.Initrd)
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "#!ipxe\n")
	fmt.Fprintf(&b, "# mammoth boot entry mac=%s task=%s kind=%s (wimboot/WinPE)\n", e.MAC, e.TaskID, e.Kind)
	fmt.Fprintf(&b, "kernel %swimboot\n", base)
	for _, n := range names {
		fmt.Fprintf(&b, "initrd %s%s %s\n", base, n, n)
	}
	b.WriteString("boot\n")
	return b.String()
}

// NoEntryScript is served when a MAC has no pending boot entry. iPXE must
// exit rather than stop in its shell: exit hands control back to the
// firmware, which falls through to the next boot device (the local disk).
// This is what makes the post-install race harmless — a firmware that
// PXE-boots again after the entry was released simply boots from disk.
func NoEntryScript(mac string) string {
	return fmt.Sprintf("#!ipxe\n# mammoth: no pending boot entry for %s\n"+
		"echo mammoth: no boot entry for this MAC\nexit\n", mac)
}

// RenderEnrollScript renders the zero-registration fallback (docs/
// 09-roadmap.md): an unknown MAC is offered the shared enrollment payload —
// an alpine probe environment that scans /sys and reports to the enrollment
// endpoint keyed by the booting NIC's MAC (kernel arg; the shared overlay
// cannot know it at build time). e is the shared tree descriptor built once
// at startup (Token fixed "enroll"; Extra carries the file grants the
// enroll-file endpoint serves).
func RenderEnrollScript(e *Entry, baseURL, mac string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#!ipxe\n")
	fmt.Fprintf(&b, "# mammoth enroll entry mac=%s\n", mac)
	fmt.Fprintf(&b, "kernel %s/netboot/enroll-file/%s %s enroll_mac=%s\n",
		strings.TrimSuffix(baseURL, "/"), e.Kernel, sanitizeArgs(e.KernelArgs), mac)
	fmt.Fprintf(&b, "initrd %s/netboot/enroll-file/%s\n",
		strings.TrimSuffix(baseURL, "/"), e.Initrd)
	b.WriteString("boot\n")
	return b.String()
}

// sanitizeArgs collapses whitespace runs so the args stay on the kernel
// line — upstream composes them from trusted templates, this only guards
// against accidental newlines breaking the script. Semicolons are escaped
// for the GRUB parser: an unescaped ";" is grub's command separator, which
// truncates the linux line at the first one (real-hardware 2288H: the
// ubuntu args "autoinstall ds=nocloud-net;s=http://... ip=... boot=casper"
// lost everything after the ";", the kernel booted with no ip=/BOOTIF/
// nfsroot at all and casper fell into its interactive prompt).
func sanitizeArgs(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), ";", `\;`)
}

// RenderGRUB renders the GRUB config for an entry (UEFI Secure Boot chain:
// shim → grubnet → grub.cfg). grubnet fetches its config from TFTP before its
// network stack is fully up, so kernel/initrd ride HTTP via grub's
// (http,host:port) device syntax — the host:port is baked in from baseURL.
// The port matters: mammoth serves the machine face on a non-standard port
// (default 8080), which grub must be told explicitly.
//
// The config is a bare command sequence (linux → initrd → boot), not a
// menuentry: under Secure Boot grubnet cannot load its terminal/font modules,
// which sends it into the interactive menu and stalls at the menu instead of
// auto-booting a timeout=0 entry. A top-level boot command never enters the
// menu at all.
func RenderGRUB(e *Entry, baseURL string) string {
	host := grubHTTPHost(baseURL)
	if e.Kernel == "wimboot" {
		// The wimboot carrier has no Secure Boot chain: grub's UEFI loaders
		// cannot hand files to a chainloaded image, and wimboot is unsigned
		// anyway (docs/compat/distros.md §windows). Exit cleanly so the
		// firmware falls through to the next boot device — the same
		// post-install race neutralization as NoEntryGRUB.
		var b strings.Builder
		fmt.Fprintf(&b, "# mammoth boot entry mac=%s task=%s kind=%s (wimboot/WinPE)\n", e.MAC, e.TaskID, e.Kind)
		fmt.Fprintf(&b, "# wimboot rides plain iPXE, not the shim→grubnet chain (Secure Boot off required)\n")
		fmt.Fprintf(&b, "echo mammoth: wimboot entry needs plain iPXE — falling through\n")
		b.WriteString("exit\n")
		return b.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth boot entry mac=%s task=%s kind=%s\n", e.MAC, e.TaskID, e.Kind)
	// grubnet already brought up efinet to fetch this file over TFTP; a
	// net_bootp here would re-DHCP every interface and fail on the second NIC
	// (efinet1), which aborts the config. The kernel/initrd ride grub's
	// (http,server:port) device syntax — NOT http:// URLs, which grub's
	// loader treats as a bare TFTP filename.
	fmt.Fprintf(&b, "linux (http,%s)/netboot/files/%s/%s %s\n",
		host, e.Token, e.Kernel, sanitizeArgs(e.KernelArgs))
	fmt.Fprintf(&b, "initrd (http,%s)/netboot/files/%s/%s\n", host, e.Token, e.Initrd)
	b.WriteString("boot\n")
	return b.String()
}

// NoEntryGRUB is served when a MAC has no pending boot entry. GRUB's exit
// returns control to the firmware, which falls through to the next boot
// device (the local disk) — the same post-install race neutralization as the
// iPXE path.
func NoEntryGRUB(mac string) string {
	return fmt.Sprintf("# mammoth: no pending boot entry for %s\nexit\n", mac)
}

// grubHTTPHost extracts host[:port] from a base URL for grub's
// (http,host:port) device syntax.
func grubHTTPHost(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
}

// grubConfigMAC parses the per-MAC GRUB config filename grubnet requests —
// "grub.cfg-01-<mac>" with the mac in lowercase colon form — and returns the
// normalized mac. grubnet falls back to "grub.cfg-<hex-ip>" then "grub.cfg"
// when the per-MAC file is absent; those fallbacks are static and never hit
// this path.
func grubConfigMAC(name string) (string, bool) {
	const prefix = "grub.cfg-01-"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	mac := strings.TrimSpace(name[len(prefix):])
	if mac == "" {
		return "", false
	}
	return normalizeMAC(mac), true
}
