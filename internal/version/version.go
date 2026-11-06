// Package version carries build metadata for the mammoth binary.
package version

import "runtime/debug"

// Build information, overridable via -ldflags at link time. Version keeps
// the git tag's leading "v" on every build path (Dockerfile git describe,
// goreleaser, manual -X): v1.3.0, or v1.3.0-29-ge9553f6 between tags.
// Consumers (capabilities, --version, MCP) show it verbatim.
var (
	Version = "v0.1.0-dev"
	Commit  = "none"
)

func init() {
	if info, ok := debug.ReadBuildInfo(); ok && Commit == "none" {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				Commit = s.Value
			}
		}
	}
}
