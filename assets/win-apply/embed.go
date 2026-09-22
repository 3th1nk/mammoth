// Package winapply embeds the windows apply-image toolchain the agent
// overlay carries onto the machine (boot.installer=agent): wimlib-imagex +
// mkntfs and their musl runtime library closure, all x86_64 musl binaries
// taken verbatim from the Alpine 3.22 package repositories — the same
// release the agent carrier runs, so no cross-version libc drift. See
// PROVENANCE.md for package versions, hashes and the refresh procedure.
package winapply

import "embed"

// Tools holds the overlay payload: usr/bin/wimlib-imagex, usr/sbin/mkntfs
// and usr/lib/{libwim.so.15, libntfs-3g.so.89, libfuse3.so.3, libuuid.so.1}.
// The overlay lands them under /usr/local/mammoth-win/ on the machine; the
// agent runtime sets LD_LIBRARY_PATH to the lib dir and calls the binaries
// by absolute path (no PATH games in the plan-driven flow).
//
//go:embed tools
var Tools embed.FS
