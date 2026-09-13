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
