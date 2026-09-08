package provision

import (
	"context"

	"github.com/3th1nk/mammoth/internal/builder"
)

// BootMediaBuilder assembles the per-task boot ISO (media B). Behind this
// interface the builder facet may run in-process (default) or remotely; the
// pipeline only cares that a bootable ISO lands in the media repository.
type BootMediaBuilder interface {
	BuildBootISO(ctx context.Context, opt builder.BootMediaOptions, kernelArgs string) (string, error)
	// EnsureISO makes the distro ISO available locally (downloads/caches
	// when sourceURL is remote; passthrough for local paths).
	EnsureISO(ctx context.Context, sourceURL, cacheDir string) (string, error)
}

// defaultBootMediaBuilder runs xorriso locally.
type defaultBootMediaBuilder struct{}

func (defaultBootMediaBuilder) BuildBootISO(ctx context.Context, opt builder.BootMediaOptions, kernelArgs string) (string, error) {
	return builder.BuildBootISO(ctx, opt, kernelArgs)
}

func (defaultBootMediaBuilder) EnsureISO(ctx context.Context, sourceURL, cacheDir string) (string, error) {
	return builder.EnsureISO(ctx, sourceURL, cacheDir)
}

// compile-time: render import reserved for future builder-side answer
// embedding (kickstart inside the media for offline installs).
