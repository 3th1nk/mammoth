// Package builder hosts the build facet: answer-file rendering coordination
// and boot-media assembly (xorriso). Media assembly runs inside the
// all/runner facets (the standalone builder placeholder mode was
// removed in v1.4.2); the deployment unit boundary (toolchain-bearing image,
// external tools) is declared in Dockerfile.builder so the architecture
// does not drift.
package builder

// CheckToolchain reports the media-assembly external tools missing from PATH.
// xorriso gates every repack/kit path (hard — the runner facet refuses to
// start without it); wimlib-imagex and 7z only serve the windows pathway
// (soft — reported as a startup warning, so linux-only deployments on a lean
// image still boot). lookPath is injected for tests. Used by serve at startup
// so a missing toolchain surfaces as a clear deployment error instead of a
// mid-install task failure (2026-11-10: the distroless mammoth image ships
// no toolchain at all and its old default CMD was --mode=all).
func CheckToolchain(lookPath func(string) (string, error)) (missingHard, missingSoft []string) {
	for _, tool := range []string{"xorriso"} {
		if _, err := lookPath(tool); err != nil {
			missingHard = append(missingHard, tool)
		}
	}
	for _, tool := range []string{"wimlib-imagex", "7z"} {
		if _, err := lookPath(tool); err != nil {
			missingSoft = append(missingSoft, tool)
		}
	}
	return missingHard, missingSoft
}
