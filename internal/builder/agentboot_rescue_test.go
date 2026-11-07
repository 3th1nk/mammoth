package builder

// The agent runtime's rescue payload (docs/07-bmc.md §6.5, roadmap 下一阶段
// 11): the dispatch branches on MAMMOTH_RESCUE before the declarative
// install chain — no install steps run in rescue mode; sshd comes from the
// staged pool, the operator's keys land in root's authorized_keys, and the
// runtime reports rescue-ready then opens the console shell WITHOUT
// rebooting (rescue is long-lived by design).

import (
	"strings"
	"testing"
)

func TestRuntimeRescueDispatch(t *testing.T) {
	script := agentScript()
	if !strings.Contains(script, `if [ "$MAMMOTH_RESCUE" = "1" ]; then`) {
		t.Errorf("runtime has no MAMMOTH_RESCUE dispatch branch")
	}
	// The branch must run after the dd branch (dispatch order: win → dd →
	// rescue → declarative install) and before the install chain.
	idxRescue := strings.Index(script, `if [ "$MAMMOTH_RESCUE" = "1" ]; then`)
	idxDD := strings.Index(script, `if [ "$MAMMOTH_DD_MODE" = "1" ]; then`)
	idxInstall := strings.Index(script, "bootstrap_tools || bail tools")
	if idxRescue < idxDD || (idxInstall >= 0 && idxRescue > idxInstall) {
		t.Errorf("rescue dispatch out of order (dd at %d, rescue at %d, install chain at %d)",
			idxDD, idxRescue, idxInstall)
	}
	for _, want := range []string{
		// sshd from the pool; absence degrades to console-only — honest
		// warning, not a failure.
		"apk add --no-cache openssh",
		"console-only rescue",
		// the operator's way in: root's authorized_keys from the plan
		"MAMMOTH_SSH_KEYS",
		"/root/.ssh/authorized_keys",
		"ssh-keygen -A",
		"/usr/sbin/sshd",
		// rescue-ready report, then the long-lived console shell — NOT
		// exec (the log shipper must keep running) and NOT reboot.
		`report ok "rescue ready`,
		"rescue console open",
		"        /bin/sh",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rescue branch missing %q", want)
		}
	}
	// The rescue branch must not reboot: a reboot would kill the rescue
	// session the operator is using.
	rescueBlock := script[idxRescue:idxInstall]
	if strings.Contains(rescueBlock, "reboot -f") || strings.Contains(rescueBlock, "\nreboot") {
		t.Errorf("rescue branch must not reboot:\n%s", rescueBlock)
	}
}
