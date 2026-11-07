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

// The agent runtime's bootc payload (docs/09-roadmap.md 下一阶段 16): the
// dispatch branches on MAMMOTH_BOOTC after dd — podman from the pool, pull,
// then bootc install to-disk inside the image's own container (bootc's
// documented shape); keys ride --root-ssh-authorized-keys; the report is
// "applied" (no first-boot callback — the image's boot is its own business).
func TestRuntimeBootcDispatch(t *testing.T) {
	script := agentScript()
	if !strings.Contains(script, `if [ "$MAMMOTH_BOOTC" = "1" ]; then`) {
		t.Errorf("runtime has no MAMMOTH_BOOTC dispatch branch")
	}
	idxBootc := strings.Index(script, `if [ "$MAMMOTH_BOOTC" = "1" ]; then`)
	idxDD := strings.Index(script, `if [ "$MAMMOTH_DD_MODE" = "1" ]; then`)
	idxRescue := strings.Index(script, `if [ "$MAMMOTH_RESCUE" = "1" ]; then`)
	if idxBootc < idxDD || (idxRescue >= 0 && idxBootc > idxRescue) {
		t.Errorf("bootc dispatch out of order (dd at %d, bootc at %d, rescue at %d)",
			idxDD, idxBootc, idxRescue)
	}
	for _, want := range []string{
		// podman from the extended pool — the pathway's hard prerequisite,
		// with an honest error naming the deployment knob
		"apk add --no-cache podman",
		"MAMMOTH_WINDOWS_APPLY_ALPINE_ISO",
		"podman pull",
		// bootc's documented container-install shape (privileged, host
		// /dev, the image's own storage bind)
		"--privileged --pid=host --ipc=host",
		"-v /var/lib/containers:/var/lib/containers",
		"bootc install to-disk",
		"--root-ssh-authorized-keys",
		// applied, not ok — the appliedTerminalDD gate (bootc marker
		// included) completes the task on this report
		`report applied "bootc image installed`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootc branch missing %q", want)
		}
	}
	// No read-back sha256: a deployment-style install has no byte-image to
	// hash — the digest-pinned pull is the integrity claim.
	bootcBlock := script[idxBootc:idxRescue]
	if strings.Contains(bootcBlock, "sha256sum") {
		t.Errorf("bootc branch must not carry the dd read-back hash:\n%s", bootcBlock)
	}
}
