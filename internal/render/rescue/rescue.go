// Package rescue renders the rescue-mode plan the agent runtime sources
// (docs/07-bmc.md §6.4, docs/09-roadmap.md 下一阶段 11): boot_rescue boots
// the machine into the engine's in-memory alpine rescue system — sshd up,
// the operator's keys injected, NO install steps run. The plan's executable
// face follows the dd plan's shape (render/dd): MAMMOTH_* declarations the
// agent dispatches on, one line per fact, quote-safe, readable in a BMC SOL
// session.
package rescue

import (
	"fmt"
	"strings"
)

// Inputs are the rescue plan's render facts. URLs come from the executor's
// token-scoped render face (the same /render/{token}/... surface installs
// report through); keys are the request's authorized_keys-format entries.
type Inputs struct {
	TaskToken string
	MachineID string
	// CompleteURL is the rescue-ready callback (POST /render/{token}/complete
	// — the install completion endpoint; the handler branches on the task's
	// flow name). The agent reports once, when sshd is up.
	CompleteURL string
	// AgentLogURL is the runtime log shipper's target (POST
	// /render/{token}/agentlog) — rescue keeps shipping its own log tail,
	// the operator's console session is part of the record.
	AgentLogURL string
	// SSHPublicKeys are injected into the rescue system's root account
	// (authorized_keys format). Empty = console-only rescue.
	SSHPublicKeys []string
}

// Render emits the plan (agent-plan.sh content the executor seeds onto the
// boot media).
func Render(in Inputs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# mammoth rescue plan — task %s / machine %s\n", in.TaskToken, in.MachineID)
	fmt.Fprintf(&b, "MAMMOTH_RESCUE=1\n")
	fmt.Fprintf(&b, "MAMMOTH_COMPLETE_URL=%s\n", shQuote(in.CompleteURL))
	fmt.Fprintf(&b, "MAMMOTH_LOG_URL=%s\n", shQuote(in.AgentLogURL))
	fmt.Fprintf(&b, "MAMMOTH_SSH_KEYS=%s\n", shQuote(strings.Join(in.SSHPublicKeys, "\n")))
	return b.String()
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
