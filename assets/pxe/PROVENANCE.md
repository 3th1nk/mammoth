# iPXE binaries — provenance

The three network boot programs in this directory are prebuilt iPXE images
from the Debian `ipxe` package. iPXE is GPL-2.0-or-later; distributing the
binaries inside an Apache-2.0 project is covered by the GPL plus the iPXE
binary distribution exception (see `LICENSE-ipxe.txt` for both texts and
NOTICE for the summary).

## Source

| | |
|---|---|
| Package | `ipxe` **2.0.0+dfsg-5** (arch-indep, contains the prebuilt ROMs/EFIs) |
| .deb | <https://deb.debian.org/debian/pool/main/i/ipxe/ipxe_2.0.0+dfsg-5_all.deb> |
| .deb sha256 | `f0f2b3df949f3207e2ca5c1324d23d0ad94a968c3d53e18ed8ea57b12f24a4bb` |
| Upstream source | <https://deb.debian.org/debian/pool/main/i/ipxe/ipxe_2.0.0+dfsg.orig.tar.xz> |
| orig sha256 | `c408ac5366b3cc7c158b20b323dfeb53863ff66110214b02af4eab7e04dad44a` |
| Upstream | git://git.ipxe.org/ipxe.git (Debian dfsg snapshot) |
| Timeless link | <https://snapshot.debian.org/file/f0f2b3df949f3207e2ca5c1324d23d0ad94a968c3d53e18ed8ea57b12f24a4bb/> |

Note on the `+dfsg` suffix: Debian's packaging strips drivers that load
firmware blobs without source (`bnx2*`, `linda*`) and `contrib` — the
binaries here are built from that DFSG-clean tree.

## Files

| This directory | Extracted from | sha256 |
|---|---|---|
| `undionly.kpxe` | `/usr/lib/ipxe/undionly.kpxe` (BIOS chainloader, rides the NIC's UNDI ROM) | `cd72b40cc08d4f4489335232817fa0602c9dd8320eb42066f908f65eea1a3a07` |
| `ipxe-amd64.efi` | `/usr/lib/ipxe/ipxe-amd64.efi` (UEFI x64) | `548f6fc510131b10d3f77a2b9fbdedf2f00308ae5e03ecb16b66a2a6b9665408` |
| `ipxe-arm64.efi` | `/usr/lib/ipxe/ipxe-arm64.efi` (UEFI aarch64) | `f9639206ce233e495ea8dd727ab0056420a8b691161f2dfcfcc89a5acf680944` |

Build configuration is Debian's: serial console enabled, no interactive
shell, all commands available (`make EMBED=...` not used — the boot script
is fetched over HTTP at runtime, not embedded).

## Rebuild / upgrade

```sh
scripts/fetch-pxe-bins.sh          # same version, verifies sha256
scripts/fetch-pxe-bins.sh 2.0.7    # bump: pass a new Debian package version
```

The script downloads the pinned .deb, verifies its sha256, extracts the
three files, rewrites this table, and prints the diff to review. After a
bump, update `NOTICE` if the license text changed.

## Secure Boot binaries (shim + grubnet)

The UEFI x64 Secure Boot chain replaces the unsigned `ipxe-amd64.efi` with
`shimx64.efi` (Microsoft-signed shim) loading `grubx64.efi` (Debian-signed
GRUB network boot). Both come from the Debian `shim-signed` and
`grub-efi-amd64-signed` packages (current stable, trixie). shim and GRUB
must come from the same distro — the shim's embedded CA verifies GRUB's
signature, so a cross-distro mix breaks the chain.

### Source

| | |
|---|---|
| shim package | `shim-signed` **1.51~1+deb13u1+16.1-2~deb13u1** |
| shim .deb | <https://deb.debian.org/debian/pool/main/s/shim-signed/shim-signed_1.51~1+deb13u1+16.1-2~deb13u1_amd64.deb> |
| shim .deb sha256 | `3c802fa303c0e6bf126adee74028d1042e3120360d159c2c05181dc9b2f61005` |
| grub package | `grub-efi-amd64-signed` **1+2.12+9+deb13u2** |
| grub .deb | <https://deb.debian.org/debian/pool/main/g/grub-efi-amd64-signed/grub-efi-amd64-signed_1+2.12+9+deb13u2_amd64.deb> |
| grub .deb sha256 | `da8bb31308a3682a7d9343fc42bf7f603ad85bb5483eeb3ece23e2f1be2e9bbd` |

### Files

| This directory | Extracted from | sha256 |
|---|---|---|
| `shimx64.efi` | `/usr/lib/shim/shimx64.efi.signed` (Microsoft-signed shim, UEFI x64) | `e103c5d02657879f141b086ed703584d28bb9e2721b423333be085ee1b4d397b` |
| `grubx64.efi` | `/usr/lib/grub/x86_64-efi-signed/grubnetx64.efi.signed` (Debian-signed GRUB network boot; renamed for shim's second-stage lookup) | `193a143635fa41834232954a9efae4b7c61488cc5025e803ea32deca9c4f9200` |

### Licensing

- **shim**: GPL-2.0-only — Copyright Red Hat, Inc. and contributors.
- **GRUB**: GPL-3.0-or-later — Copyright Free Software Foundation, Inc. and
  contributors.

### Rebuild

```sh
scripts/fetch-secureboot-bins.sh                 # same versions, verifies sha256
scripts/fetch-secureboot-bins.sh <shim> <grub>   # bump: pass new Debian package versions
```

After a bump, update the two `.deb` sha256 above and `NOTICE` if the license
text changed.
