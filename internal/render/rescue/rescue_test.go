package rescue

import (
	"strings"
	"testing"
)

// The rescue plan is the agent's executable face (render/dd renderSH shape):
// MAMMOTH_RESCUE dispatch flag, token-scoped report/log URLs, the operator's
// keys as one newline-joined single-quoted declaration.
func TestRender(t *testing.T) {
	plan := Render(Inputs{
		TaskToken:     "tok123",
		MachineID:     "mach-1",
		CompleteURL:   "http://engine:8091/render/tok123/complete",
		AgentLogURL:   "http://engine:8091/render/tok123/agentlog",
		SSHPublicKeys: []string{"ssh-ed25519 AAA operator@host", "ssh-rsa AAA second@host"},
	})
	for _, want := range []string{
		"MAMMOTH_RESCUE=1",
		`MAMMOTH_COMPLETE_URL='http://engine:8091/render/tok123/complete'`,
		`MAMMOTH_LOG_URL='http://engine:8091/render/tok123/agentlog'`,
		"ssh-ed25519 AAA operator@host\nssh-rsa AAA second@host",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "MAMMOTH_DD_MODE") || strings.Contains(plan, "MAMMOTH_WIN_MODE") {
		t.Errorf("rescue plan must not carry other pathways' dispatch flags:\n%s", plan)
	}
}

// Quote safety: a single quote in a key must round-trip through the shQuote
// form — the plan is sourced by busybox sh and one broken quote kills the
// runtime before the report.
func TestRenderQuoteSafety(t *testing.T) {
	plan := Render(Inputs{
		TaskToken:     "tok",
		MachineID:     "m",
		CompleteURL:   "http://e/render/tok/complete",
		AgentLogURL:   "http://e/render/tok/agentlog",
		SSHPublicKeys: []string{"ssh-ed25519 AAA comment'with'quotes@host"},
	})
	if !strings.Contains(plan, `comment'\''with'\''quotes`) {
		t.Errorf("single quotes not escaped:\n%s", plan)
	}
}
