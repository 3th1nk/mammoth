package builder

import (
	"io/fs"
	"testing"
)

func TestCheckToolchainAllPresent(t *testing.T) {
	ok := func(string) (string, error) { return "/usr/bin/tool", nil }
	hard, soft := CheckToolchain(ok)
	if len(hard) != 0 || len(soft) != 0 {
		t.Fatalf("complete toolchain must report nothing, got hard=%v soft=%v", hard, soft)
	}
}

func TestCheckToolchainDistrolessShape(t *testing.T) {
	// The distroless mammoth image ships no toolchain at all — xorriso is
	// the hard gate, wimlib/7z the soft windows-only report.
	missing := func(string) (string, error) { return "", fs.ErrNotExist }
	hard, soft := CheckToolchain(missing)
	if len(hard) != 1 || hard[0] != "xorriso" {
		t.Fatalf("xorriso must be the hard gate, got hard=%v", hard)
	}
	if len(soft) != 2 {
		t.Fatalf("wimlib/7z must be soft findings, got soft=%v", soft)
	}
}

func TestCheckToolchainLinuxOnlyLean(t *testing.T) {
	// xorriso present, windows tools absent: boot must succeed (soft only).
	calls := 0
	lean := func(tool string) (string, error) {
		calls++
		if tool == "xorriso" {
			return "/usr/bin/xorriso", nil
		}
		return "", fs.ErrNotExist
	}
	hard, soft := CheckToolchain(lean)
	if len(hard) != 0 {
		t.Fatalf("xorriso present must not hard-fail, got hard=%v", hard)
	}
	if len(soft) != 2 || calls != 3 {
		t.Fatalf("lean image must soft-report windows tools, got soft=%v calls=%d", soft, calls)
	}
}
