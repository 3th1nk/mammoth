// Package builder hosts the build facet: answer-file rendering coordination
// and boot-media assembly (xorriso). It arrives with the M3 install pipeline
// (docs/09-roadmap.md); the deployment unit boundary (privileged container,
// external tools) is declared from M0 so the architecture does not drift.
package builder
