package builder

import (
	"os/exec"
	"strings"
	"testing"
)

// The probe script is generated busybox-sh; a syntax error here only
// surfaces on real hardware as a probe that drops to a shell. sh -n parses
// it without executing (POSIX sh on the dev host, busybox ash on target —
// the portable subset).
func TestProbeScriptSyntax(t *testing.T) {
	script := probeScript("https://m/render/tok/probe-report", "", "", "lts")
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe script has shell syntax errors: %v\n%s", err, out)
	}
}

// Health collection (docs/04-install-spec.md §5.6): the tooling pass is
// best-effort (carrier-dependent), the per-disk verdict is emitted only
// when a tool answers, and absence emits nothing — the gate's "no evidence,
// no intercept" contract starts here.
func TestProbeScriptHealth(t *testing.T) {
	script := probeScript("https://m/render/tok/probe-report", "", "", "lts")
	for _, want := range []string{
		"apk add --quiet --no-network smartmontools nvme", // best-effort, boot repo only
		"smartctl -H",      // preferred verdict tool
		"critical_warning", // nvme fallback
		"disk_health",
		`\"health\":\"$h\"`, // emitted only when non-empty
	} {
		if !strings.Contains(script, want) {
			t.Errorf("probe script missing health machinery %q", want)
		}
	}
}
