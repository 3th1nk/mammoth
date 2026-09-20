// Package pxe embeds the network boot programs served over TFTP by the
// netboot service (internal/netboot). The iPXE binaries come from the Debian
// `ipxe` package; the Secure Boot chain (shim + grubnet) comes from the
// `shim-signed` and `grub-efi-amd64-signed` packages — see PROVENANCE.md for
// source, hashes and the rebuild procedure; LICENSE-ipxe.txt carries the
// GPL-2.0 text plus the iPXE binary distribution exception.
package pxe

import "embed"

// Files holds the NBP binaries: undionly.kpxe (BIOS), ipxe-amd64.efi and
// ipxe-arm64.efi (UEFI, unsigned — plain UEFI only), the Secure Boot chains
// shimx64.efi + grubx64.efi (UEFI x64) and shimaa64.efi + grubaa64.efi (UEFI
// aarch64, all Microsoft/Debian signed), wimboot (the WinPE chain loader the
// Windows carrier serves as an iPXE "kernel"), and grubnet's module-list
// tables under grub/x86_64-efi/ and grub/arm64-efi/ (command/crypto/fs/
// terminal.lst etc.) that grubnet fetches from its (tftp)/grub/ prefix.
//
//go:embed undionly.kpxe ipxe-amd64.efi ipxe-arm64.efi shimx64.efi grubx64.efi shimaa64.efi grubaa64.efi wimboot grub/x86_64-efi grub/arm64-efi
var Files embed.FS
