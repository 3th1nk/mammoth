package netboot

import (
	"context"
	"fmt"
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
func (e *Entry) FileNames() []string {
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
func RenderScript(e *Entry, baseURL string) string {
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

// NoEntryScript is served when a MAC has no pending boot entry. iPXE must
// exit rather than stop in its shell: exit hands control back to the
// firmware, which falls through to the next boot device (the local disk).
// This is what makes the post-install race harmless — a firmware that
// PXE-boots again after the entry was released simply boots from disk.
func NoEntryScript(mac string) string {
	return fmt.Sprintf("#!ipxe\n# mammoth: no pending boot entry for %s\n"+
		"echo mammoth: no boot entry for this MAC\nexit\n", mac)
}

// sanitizeArgs collapses whitespace runs so the args stay on the kernel
// line — upstream composes them from trusted templates, this only guards
// against accidental newlines breaking the script.
func sanitizeArgs(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
