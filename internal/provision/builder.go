package provision

import (
	"context"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/builder"
)

// mediaURIFor returns the BMC-accessible URI for a media file name.
func mediaURIFor(base, filename string) string {
	if base == "" {
		return ""
	}
	return strings.TrimSuffix(base, "/") + "/" + filename
}

// ensureISO and buildBootISO wrap the builder package for the Executor.
func ensureISO(ctx context.Context, sourceURL, cacheDir string) (string, error) {
	return builder.EnsureISO(ctx, sourceURL, cacheDir)
}

func buildBootISO(ctx context.Context, isoPath, outputPath, kernelArgs string, seedFiles map[string]string, workDir, cacheDir string) error {
	_, err := builder.BuildBootISO(ctx, builder.BootMediaOptions{
		ISOPath:    isoPath,
		OutputPath: outputPath,
		Timeout:    30 * time.Minute, // windows cache-miss chain: extract+wim+assembly ≈ 9 min on the deployment host
		SeedFiles:  seedFiles,
		WorkDir:    workDir,
		CacheDir:   cacheDir,
	}, kernelArgs)
	return err
}
