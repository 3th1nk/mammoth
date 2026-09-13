// Package pxe embeds the iPXE network boot programs served over TFTP by the
// netboot service (internal/netboot). The binaries come from the Debian
// `ipxe` package — see PROVENANCE.md for source, hashes and the rebuild
// procedure; LICENSE-ipxe.txt carries the GPL-2.0 text plus the iPXE binary
// distribution exception.
package pxe

import "embed"

// Files holds the NBP binaries: undionly.kpxe (BIOS), ipxe-amd64.efi
// (UEFI x64), ipxe-arm64.efi (UEFI aarch64).
//
//go:embed undionly.kpxe ipxe-amd64.efi ipxe-arm64.efi
var Files embed.FS
