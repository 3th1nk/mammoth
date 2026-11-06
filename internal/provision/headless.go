package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/inventory/inbandssh"
	"github.com/3th1nk/mammoth/internal/obs"
)

// Headless boot (docs/13-headless-design.md §4): a protocol=none machine
// has no BMC to point at PXE and no power state to read, so the boot stage
// arms the netboot payload (prepare, already done) and then works from
// EVIDENCE instead of power callbacks:
//
//  1. a graceful in-band reboot when the machine has an installed system
//     with the mammoth SSH credential (the reinstall path);
//  2. Wake-on-LAN magic packets otherwise (engine direct-send, or the
//     edge(原 edge) relay in the cross-segment shape);
//  3. a wait for boot evidence — the machine's PXE sighting after arm time
//     (builtin mode: the proxyDHCP observation columns; external mode: the
//     proxy's sighting feed into pending_machines);
//  4. the manual fallback: past the spec-tunable evidence window the stage
//     warns the operator to power the machine on and KEEPS WAITING — a
//     power-on after the warning self-heals the flow. The overall cap is
//     the task timeout, so an operator's window is bounded and retries
//     re-enter the same wait.
//
// The wait lives in the boot stage deliberately: boot's contract is "the
// machine is in the installer", and on a headless machine that claim needs
// evidence, not trust.

const (
	// defaultBootEvidenceWait is the evidence window (boot.evidence_wait_
	// seconds absent): enough for WoL + firmware PXE init on the slowest
	// commodity BIOS, short enough that the manual warning arrives while
	// an operator is still around.
	defaultBootEvidenceWait = 10 * time.Minute
	// bootEvidenceTick paces the evidence poll (the PXE observation is
	// written on DISCOVER, so the loop latency is one DHCP round).
	bootEvidenceTick = 5 * time.Second
	// bootEvidenceClockSkew tolerates engine↔wire clock differences when
	// comparing sighting timestamps against the arm-time watermark.
	bootEvidenceClockSkew = 10 * time.Second
)

// armHeadless drives a protocol=none machine into the installer.
func (e *Executor) armHeadless(ctx context.Context, b *bootSession) error {
	task, job, ictx := b.Task, b.Job, b.Ictx
	log := obs.FromContext(ctx)
	macs := ictx.Netboot.ArmedMACs()
	if len(macs) == 0 {
		return classifiedErr("NETBOOT_MAC_UNAVAILABLE", false,
			"headless machine %s has no armed MACs (the netboot registry is keyed by MAC)", task.MachineID)
	}

	// The evidence watermark predates arm slightly: a DISCOVER that raced
	// the arm must still count, a sighting from the previous install must
	// not (skew covers the write latency between wire and column).
	t0 := time.Now().Add(-bootEvidenceClockSkew)

	// 1. Graceful in-band reboot for the reinstall path. Best-effort: a
	// fresh machine has no reachable OS yet, and an unreachable one is the
	// WoL/manual path's job — never a boot failure.
	if err := e.tryInbandReboot(ctx, task.MachineID); err != nil {
		log.InfoContext(ctx, "in-band reboot unavailable; WoL/manual power-on next",
			obs.FieldTaskID, task.ID, "err", err.Error())
	} else {
		log.InfoContext(ctx, "graceful in-band reboot issued", obs.FieldTaskID, task.ID)
	}

	// 2. Wake-on-LAN for the powered-off path. Best-effort the same way:
	// deployments without a WoL route (no broadcast address, cross-segment
	// without the proxy relay) lean on the manual fallback below.
	if e.WakeOnLAN != nil {
		for _, mac := range macs {
			if err := e.WakeOnLAN(mac); err != nil {
				log.WarnContext(ctx, "Wake-on-LAN send failed; manual power-on is the fallback",
					obs.FieldTaskID, task.ID, "mac", mac, "err", err.Error())
			}
		}
	} else {
		log.InfoContext(ctx, "Wake-on-LAN not configured; manual power-on is the fallback",
			obs.FieldTaskID, task.ID)
	}

	window := defaultBootEvidenceWait
	if b.Spec != nil && b.Spec.Boot.EvidenceWaitSeconds > 0 {
		window = time.Duration(b.Spec.Boot.EvidenceWaitSeconds) * time.Second
	}
	if e.HeadlessBootWait > 0 {
		window = e.HeadlessBootWait
	}
	cap_ := job.Policy.TaskTimeout()
	_ = e.Jobs.RecordInstallProgress(ctx, task.ID, map[string]any{"boot_wait_started_at": time.Now().UTC()})
	e.Events.Append(ctx, "task", task.ID, "task.boot_wait_started", map[string]any{
		"macs": macs, "window_seconds": int(window.Seconds()),
	})
	log.InfoContext(ctx, "waiting for boot evidence (PXE sighting after arm)",
		obs.FieldTaskID, task.ID, "macs", macs, "window", window.String())

	// 3.+4. The evidence loop: poll, warn once past the window, cap at the
	// task timeout. Evidence from ANY armed MAC counts — which port the
	// firmware boots is a firmware-order decision the engine doesn't see.
	warned := false
	deadline := time.Now().Add(cap_)
	for {
		if e.hasBootEvidence(ctx, task.MachineID, macs, t0) {
			now := time.Now().UTC()
			_ = e.Jobs.RecordInstallProgress(ctx, task.ID, map[string]any{"booted_at": now})
			log.InfoContext(ctx, "boot evidence observed — machine is in the installer",
				obs.FieldTaskID, task.ID)
			return nil
		}
		if !warned && time.Since(t0) > window {
			warned = true
			log.WarnContext(ctx, "no boot evidence within the window — power the machine on manually (PXE boot); the flow self-heals when it appears",
				obs.FieldTaskID, task.ID, "macs", macs)
			e.Events.Append(ctx, "task", task.ID, "task.boot_manual_required", map[string]any{
				"macs": macs, "window_seconds": int(window.Seconds()),
			})
		}
		if time.Now().After(deadline) {
			return classifiedErr("BOOT_EVIDENCE_TIMEOUT", true,
				"no boot evidence within %s — is the machine powered on and set to boot PXE first?", cap_)
		}
		tick := bootEvidenceTick
		if e.evidenceTick > 0 {
			tick = e.evidenceTick
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(tick):
		}
	}
}

// hasBootEvidence reports whether the machine was SEEN on the install L2
// after since. Two sources, both existing plumbing: the PXE responder's
// observation columns (builtin mode — the proxyDHCP DISCOVER fires
// ObservePXE, which matches the machine's hardware nics) and the pending
// sightings ledger (external mode — the segment proxy's sighting feed lands
// here regardless of registration). DHCP leases are deliberately NOT
// evidence: pool leases are sticky and survive from a previous install.
func (e *Executor) hasBootEvidence(ctx context.Context, machineID string, macs []string, since time.Time) bool {
	if m, err := e.Machines.Get(ctx, machineID); err == nil &&
		m.PXELastSeenAt != nil && m.PXELastSeenAt.After(since) {
		return true
	}
	if e.Pending != nil {
		for _, mac := range macs {
			if p, err := e.Pending.Get(ctx, mac); err == nil && p.LastSeenAt.After(since) {
				return true
			}
		}
	}
	return false
}

// inbandRebootScript detaches the actual reboot from the SSH session: the
// command returns immediately ("triggered"), the machine restarts a second
// later — a torn-down connection after a synchronous `reboot` is
// indistinguishable from a dial failure, and the two mean OPPOSITE things
// here (rebooting vs. not reachable).
const inbandRebootScript = `nohup sh -c 'sleep 1; reboot' >/dev/null 2>&1 & echo triggered`

// tryInbandReboot gracefully reboots an INSTALLED system over SSH (the
// headless reinstall path — no BMC, but a running OS with the mammoth
// in-band credential answers). Any error means "not reachable in band":
// missing credential/address, dial/auth failure, or a command that never
// confirmed — the caller falls through to WoL / manual either way.
func (e *Executor) tryInbandReboot(ctx context.Context, machineID string) error {
	m, err := e.Machines.Get(ctx, machineID)
	if err != nil {
		return err
	}
	if m.SSHCredentialID == nil || m.SSHAddress == "" {
		return fmt.Errorf("no in-band address or credential")
	}
	credRow, err := e.Credentials.Get(ctx, *m.SSHCredentialID)
	if err != nil {
		return err
	}
	if credRow.Type != "ssh" {
		return fmt.Errorf("credential %q is not an ssh credential", *m.SSHCredentialID)
	}
	plain, err := e.Crypto.Decrypt(credRow.SecretEncrypted)
	if err != nil {
		return fmt.Errorf("credential decrypt failed")
	}
	var secret struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(plain, &secret); err != nil {
		return fmt.Errorf("credential payload malformed")
	}
	out, err := e.Inband.Runner.Run(ctx, m.SSHAddress, inbandssh.Credentials{
		Username: secret.Username, Password: secret.Password, PrivateKey: secret.PrivateKey,
	}, inbandRebootScript)
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "triggered") {
		return fmt.Errorf("reboot command did not confirm")
	}
	return nil
}
