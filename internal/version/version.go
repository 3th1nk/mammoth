// Package version carries build metadata for the mammoth binary.
package version

import "runtime/debug"

// Build information, overridable via -ldflags at link time.
var (
	Version = "0.1.0-dev"
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
