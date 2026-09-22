# win-apply tools — provenance

Embedded binaries for the windows agent apply-image pathway
(`boot.installer=agent`): the agent overlay carries them onto the machine
and the runtime invokes them directly (`LD_LIBRARY_PATH` → `usr/lib`).

All files are taken VERBATIM from the Alpine Linux 3.22 package
repositories (`https://dl-cdn.alpinelinux.org/alpine/v3.22/`) — the same
release as the agent carrier (alpine netboot tarball), so the binaries run
against the exact musl they were built for. Each package is signature-
verified by apk at fetch time (`.SIGN.RSA.alpine-devel@...`).

| file | package | upstream |
|---|---|---|
| usr/bin/wimlib-imagex | community/wimlib-1.14.4-r0 | https://wimlib.net (GPL-2.0+) |
| usr/lib/libwim.so.15 | community/wimlib-libs-1.14.4-r0 | LGPL-2.1+ |
| usr/sbin/mkntfs | main/ntfs-3g-progs-2026.2.25-r0 | https://www.tuxera.com GPL-2.0+ |
| usr/lib/libntfs-3g.so.89 | main/ntfs-3g-libs-2026.2.25-r0 | GPL-2.0+ |
| usr/lib/libfuse3.so.3 | main/fuse3-libs-3.17.3-r0 (wimlib's wimmount support links it; unused by apply) | LGPL-2.1 |
| usr/lib/libuuid.so.1 | main/libuuid-2.41-r0 (mkntfs runtime dep) | BSD-3 |

Versions are PINNED: the refresh procedure re-runs the fetch script, which
fails loudly when a pinned version disappears from the mirror (update the
pinned versions deliberately, re-run the qemu boot-spike, then commit).

## refresh

```sh
scripts/fetch-win-apply-tools.sh
```

Downloads the pinned .apk files, verifies sha256 against this file's
version table (update hashes when bumping versions), extracts the files
above, and re-signs nothing (macOS quarantine does not apply to committed
artifacts; on macOS the extracted binaries are committed as-is).

## scope

x86_64 musl only — the windows driver is amd64-only (render.SupportedArchs).
The overlay carries these for every windows agent task regardless of target
(~0.9 MiB compressed inside the apkovl).

License note: wimlib/ntfs-3g are GPL — these binaries are aggregated, not
linked, with mammoth (Apache-2.0); they are used as separate programs, and
their source is available at the upstream URLs above.
