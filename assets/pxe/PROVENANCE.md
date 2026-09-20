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
| `grub/x86_64-efi/*.lst` | `/usr/lib/grub/x86_64-efi/*.lst` (module-list tables for grubnet's `(tftp)/grub/` prefix) | per-file content of the trixie `grub-efi-amd64-bin` .deb (version not pinned when first added; re-align via the fetch script's module-tables step) |

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

### Arm64 chain (shimaa64 + grubnetaa64)

The UEFI aarch64 Secure Boot chain mirrors x64 exactly: `shimaa64.efi`
(Microsoft-signed shim) loads `grubaa64.efi` (Debian-signed GRUB network
boot, renamed from `grubnetaa64.efi.signed` for shim's second-stage
lookup). Same distro pairing rule as x64 — shim and GRUB must come from the
same distro.

| | |
|---|---|
| shim package | `shim-signed` **1.51~1+deb13u1+16.1-2~deb13u1** (arm64) |
| shim .deb | <https://deb.debian.org/debian/pool/main/s/shim-signed/shim-signed_1.51~1+deb13u1+16.1-2~deb13u1_arm64.deb> |
| shim .deb sha256 | `e228a68b298865e0f1b35f93dac8fc9644a47bde3487419efe305777e2d4a1f0` |
| grub signed package | `grub-efi-arm64-signed` **1+2.12+9+deb13u2** (arm64) |
| grub signed .deb | <https://deb.debian.org/debian/pool/main/g/grub-efi-arm64-signed/grub-efi-arm64-signed_1+2.12+9+deb13u2_arm64.deb> |
| grub signed .deb sha256 | `488a67e0910789b701baaa9c5a6bb3091479df3d673ce01e35383773045e4eac` |
| grub module tables package | `grub-efi-arm64-bin` **2.12-9+deb13u2** (arm64) |
| module tables .deb | <https://deb.debian.org/debian/pool/main/g/grub2/grub-efi-arm64-bin_2.12-9+deb13u2_arm64.deb> |
| module tables .deb sha256 | `303d92f7c7b02bce7c57efdc2c4f0fb1bf2040133546148a373d5b4eb8b94adc` |

| This directory | Extracted from | sha256 |
|---|---|---|
| `shimaa64.efi` | `/usr/lib/shim/shimaa64.efi.signed` (Microsoft-signed shim, UEFI aarch64) | `e5446d2a09dfae3fa476e7635e3d25733afefa3c821932c0684e1f57b60a4744` |
| `grubaa64.efi` | `/usr/lib/grub/arm64-efi-signed/grubnetaa64.efi.signed` (Debian-signed GRUB network boot; renamed for shim's second-stage lookup) | `9640d7178fbe2ba1de642ef45ee4d0adc5f4baeddb0a71d1697a412bff959a0b` |
| `grub/arm64-efi/*.lst` | `/usr/lib/grub/arm64-efi/*.lst` (module-list tables for grubnet's `(tftp)/grub/` prefix) | per-file content of the pinned `grub-efi-arm64-bin` .deb |

Licensing is identical to the x64 chain (shim GPL-2.0-only Red Hat; GRUB
GPL-3.0-or-later FSF) — the same NOTICE paragraph covers both.

## wimboot (Windows WinPE chain loader)

`wimboot` is the bzImage-shaped loader the Windows PXE carrier serves to
iPXE clients (`kernel wimboot` + per-file `initrd` lines): it assembles a
WinPE memory environment from the Windows media's own files (bootmgr /
bootmgfw.efi, BCD, boot.sdi, boot.wim) and hands off to the Windows boot
manager. There is no Debian package — the upstream release binary is the
distribution artifact. It is a hybrid binary (BIOS + 64-bit UEFI from one
file) and is loaded by iPXE, so the Secure Boot chain does not involve it:
a Secure Boot firmware only accepts signed NBPs, and wimboot upstream
signs none — the Windows carrier therefore requires Secure Boot disabled
(or a site-managed MOK enrollment, a deployment-layer policy mammoth does
not own).

### Source

| | |
|---|---|
| Release | **v2.9.0** |
| Binary | <https://github.com/ipxe/wimboot/releases/download/v2.9.0/wimboot> |
| Binary sha256 | `5f067ccdc4d084d5bf77b6c853bd0f8402dfc2b4cd1b103d358993ae97fae8e3` |
| Upstream source | <https://github.com/ipxe/wimboot/archive/refs/tags/v2.9.0.tar.gz> |
| Upstream | <https://github.com/ipxe/wimboot> |

### Files

| This directory | sha256 |
|---|---|
| `wimboot` | `5f067ccdc4d084d5bf77b6c853bd0f8402dfc2b4cd1b103d358993ae97fae8e3` |

### Licensing

GPL-2.0-only — Copyright the iPXE project (Michael Brown / mcb30). The
source tarball above is the complete corresponding source for the
distributed binary; rebuild with `make` in its `src/` directory.

### Rebuild / upgrade

```sh
scripts/fetch-wimboot.sh            # same version, verifies sha256
scripts/fetch-wimboot.sh v2.10.0    # bump: pass a new upstream release tag
```

After a bump, update the sha256 above and this paragraph if the license
changed.

