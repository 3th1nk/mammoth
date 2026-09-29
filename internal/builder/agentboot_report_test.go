package builder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// extractShFunc pulls one shell function (from `name() {` to the first
// column-zero `}`) out of the agent script template.
func extractShFunc(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, name+"() {")
	if start < 0 {
		t.Fatalf("agent script has no %s() function", name)
	}
	end := strings.Index(script[start:], "\n}")
	if end < 0 {
		t.Fatalf("%s() is never closed", name)
	}
	return script[start : start+end+2]
}

// TestAgentReportBodySurvivesJSONDecoder executes the agent's report()
// under /bin/sh with an adversarial detail — quotes, backslashes, newlines,
// control bytes, DEL, multibyte UTF-8, over-length — and asserts the written
// body is always valid JSON. This is the sh-side guarantee behind the
// /render/{token}/complete contract: json.Decoder rejecting the body turns
// into a 400, and a swallowed failure report leaves the task stuck running.
func TestAgentReportBodySurvivesJSONDecoder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh execution")
	}
	dir := t.TempDir()
	reportFile := filepath.Join(dir, "report.json")

	reportFn := extractShFunc(t, agentScript(), "report")
	reportFn = strings.ReplaceAll(reportFn, "/tmp/mammoth-report.json", reportFile)

	// a detail with every character class that can break a JSON string:
	// quotes, backslashes, bare newlines, tabs, control bytes, DEL, multibyte
	// runes, and enough bulk to cross the 700-char truncation
	evil := "boom \"quoted\" back\\slash\nnewline\ttab\x01\x1f\x7f control — utf8 ✓ é" + strings.Repeat("x", 900)

	script := "log() { :; }\n" +
		reportFn + "\n" +
		"bringup_network() { return 0; }\n" +
		"wget() { return 7; }\n" +
		"sleep() { :; }\n" +
		"report failed \"$EVIL\"\n"

	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "EVIL="+evil)
	out, err := cmd.CombinedOutput()
	// report() returning non-zero is the designed all-attempts-failed path
	// with the stubbed wget — only a sh-level crash (not exit 1) is fatal here
	if err != nil && err.Error() != "exit status 1" {
		t.Fatalf("report() crashed: %v\n%s", err, out)
	}

	b, err := os.ReadFile(reportFile)
	if err != nil {
		t.Fatalf("report body not written: %v", err)
	}
	var parsed struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("report body is not valid JSON (%v): %q", err, b)
	}
	if parsed.Status != "failed" {
		t.Errorf("status = %q, want failed", parsed.Status)
	}
	for i, r := range parsed.Detail {
		if r < 0x20 || r == 0x7f {
			t.Errorf("detail byte %d is a control character: %q", i, parsed.Detail)
			break
		}
	}
	// cut -c counts characters in a UTF-8 locale and bytes under C — assert
	// the intersection (rune count), both platform behaviors satisfy it
	if rc := utf8.RuneCountInString(parsed.Detail); rc > 700 {
		t.Errorf("detail not truncated: %d runes", rc)
	}
	if !strings.Contains(parsed.Detail, "boom") || !strings.Contains(parsed.Detail, "utf8 ✓ é") {
		t.Errorf("detail lost meaningful content: %q", parsed.Detail[:minInt(120, len(parsed.Detail))])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestAgentScriptSyntax runs sh -n over the whole template: the agent script
// is a static string executed by busybox sh on every install, and a syntax
// error there is a fleet-wide install outage.
func TestAgentScriptSyntax(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh execution")
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(agentScript())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent script fails sh -n: %v\n%s", err, out)
	}
}

// TestAgentFailureDiagnostics asserts the failure path leaves evidence on the
// serial console: the report body itself (so a server-side rejection can be
// diagnosed from BMC SOL alone), the wget exit code, and named config-step
// failures instead of a bare FATAL.
func TestAgentFailureDiagnostics(t *testing.T) {
	script := agentScript()
	for _, want := range []string{
		`log "report body: $(cat /tmp/mammoth-report.json 2>/dev/null)"`,
		`report attempt $i failed (exit $?)`,
		"completion report unreachable",
		"cryptpw and openssl both failed to hash",
		"config: shadow rewrite failed",
		"mammoth-config.err",
		`tr -d '\000-\037\177'`,
		"ab-v14",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("agent script missing failure diagnostic %q", want)
		}
	}
	// the old shape must not come back: a silently discarded stderr on the
	// hash step is how the sandbox acceptance ended up with an undiagnosable
	// FATAL (config)
	if strings.Contains(script, "cryptpw -m sha512 \"$MAMMOTH_ROOT_PASSWORD\" 2>/dev/null") {
		t.Error("hash step discards cryptpw stderr again")
	}
}
