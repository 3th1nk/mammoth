package provision

import (
	"context"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/builder"
)

// mediaURIFor returns the BMC-accessible URI for a media file name.
func mediaURIFor(nfsBase, filename string) string {
	if nfsBase == "" {
		return ""
	}
	return strings.TrimSuffix(nfsBase, "/") + "/" + filename
}

// ensureISO and buildBootISO wrap the builder package for the Executor.
func ensureISO(ctx context.Context, sourceURL, cacheDir string) (string, error) {
	return builder.EnsureISO(ctx, sourceURL, cacheDir)
}

func buildBootISO(ctx context.Context, isoPath, outputPath, kernelArgs string) error {
	_, err := builder.BuildBootISO(ctx, builder.BootMediaOptions{
		ISOPath:    isoPath,
		OutputPath: outputPath,
		Timeout:    10 * time.Minute,
	}, kernelArgs)
	return err
}
