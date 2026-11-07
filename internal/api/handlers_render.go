package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
	"github.com/gin-gonic/gin"
)

// ── machine-facing render surface (docs/06-install-pipeline.md §2.1) ────────
// The installer runs inside the target machine and cannot hold a bearer
// token; the unguessable task token in the path IS the credential. These
// endpoints are public by contract (security: []).

func (s *Server) FetchAnswerFile(ctx context.Context, request gen.FetchAnswerFileRequestObject) (gen.FetchAnswerFileResponseObject, error) {
	task, err := s.Jobs.GetTaskByToken(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	var ictx struct {
		Answers []struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		} `json:"answers"`
	}
	if len(task.Context) > 0 {
		if err := json.Unmarshal(task.Context, &ictx); err != nil {
			return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
				"no rendered answers for this task yet")
		}
	}
	for _, a := range ictx.Answers {
		if a.Name == request.File {
			// Fetch audit trail as events (append-only; no context churn).
			s.Events.Append(ctx, "task", task.ID, "task.answer_fetched", map[string]any{
				"file": request.File,
			})
			return gen.FetchAnswerFile200TextResponse(a.Content), nil
		}
	}
	return nil, verrStatus(http.StatusNotFound, "RENDER_UNKNOWN_FILE",
		"answer file %q not found for this task", request.File)
}

func (s *Server) ReportInstallComplete(ctx context.Context, request gen.ReportInstallCompleteRequestObject) (gen.ReportInstallCompleteResponseObject, error) {
	task, err := s.Jobs.GetTaskByToken(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	status := "ok"
	detail := ""
	if request.Body != nil {
		if request.Body.Status != nil {
			status = string(*request.Body.Status)
		}
		if request.Body.Detail != nil {
			detail = *request.Body.Detail
		}
	}
	// The rescue flow reports through the same token-scoped surface (the
	// plan carries the same /render/{token}/complete callback): the
	// rescue-ready report persists under the context's "rescue" key — the
	// rescue stage polls it and owns the payload release + terminal state.
	if task.FlowName == "rescue" {
		if err := s.Jobs.RecordRescueReady(ctx, task.ID, status, detail); err != nil {
			return nil, err
		}
		s.Events.Append(ctx, "task", task.ID, "task.rescue_reported", map[string]any{
			"status": status, "detail": detail,
		})
		obs.FromContext(ctx).InfoContext(ctx, "rescue report received",
			obs.FieldTaskID, task.ID, "status", status)
		return gen.ReportInstallComplete204Response{}, nil
	}
	// status "applied" is the agent apply-image pathway's phase-one report:
	// the image is laid down and the machine is rebooting into first boot —
	// it releases the boot payload but does NOT complete the task; the
	// terminal completion is the first-boot callback from the installed OS.
	if status == "applied" {
		if err := s.Jobs.RecordInstallApplied(ctx, task.ID, detail); err != nil {
			return nil, err
		}
		s.Events.Append(ctx, "task", task.ID, "task.install_applied", map[string]any{
			"detail": detail,
		})
		return gen.ReportInstallComplete204Response{}, nil
	}
	if err := s.Jobs.RecordInstallComplete(ctx, task.ID, status, detail); err != nil {
		return nil, err
	}
	// The report's source address is the machine's live address — the one
	// fact no spec or addressing scheme can guarantee (DHCP, reserved or
	// declared). Recording it keeps verify_ready and later in-band probes
	// pointed at reality. Best-effort: a loopback/absent peer leaves the
	// recorded address untouched — but silently: the peer is logged on
	// every completion and a refused write warns, so a pipeline that
	// degrades verify_ready to the completion-report surface (real run:
	// the bond health check skipped for lack of an in-band target) shows
	// exactly which side lost the address.
	clientIP := ClientIPFromContext(ctx)
	ip := net.ParseIP(clientIP)
	switch {
	case ip == nil || !ip.IsPrivate() || ip.IsLoopback():
		obs.FromContext(ctx).WarnContext(ctx, "completion report peer address not recorded",
			obs.FieldMachineID, task.MachineID, "client_ip", clientIP,
			"reason", "empty, unparseable, public or loopback — verify_ready will degrade to the completion-report surface")
	default:
		// Declared-static installs: the completion report arrives from the
		// BOOT-time address (agent's DHCP/lease during the pipeline), while
		// the installed system comes up on the DECLARED address — recording
		// the report's peer here would point verify_ready and every later
		// in-band probe at a dead lease (2288H 2026-11-08: PXE agent install
		// with a declared .221 recorded the boot lease .195; the new system
		// on .221 was invisible to verify_ready). The declared address is
		// deterministic — it wins over the report peer.
		if declared, ok := s.declaredStaticIP(ctx, task.JobID); ok && declared != ip.String() {
			obs.FromContext(ctx).InfoContext(ctx, "completion report peer not recorded — spec declares a static address",
				obs.FieldMachineID, task.MachineID, "report_ip", ip.String(), "declared", declared)
		} else {
			if uerr := s.Machines.SetSSHAddress(ctx, task.MachineID, ip.String()); uerr != nil {
				obs.FromContext(ctx).WarnContext(ctx, "machine ssh address write failed",
					obs.FieldMachineID, task.MachineID, "ssh_address", ip.String(), "err", uerr.Error())
			} else {
				obs.FromContext(ctx).InfoContext(ctx, "machine ssh address refreshed from completion report",
					obs.FieldMachineID, task.MachineID, "ssh_address", ip.String())
			}
		}
	}
	s.Events.Append(ctx, "task", task.ID, "task.install_reported", map[string]any{
		"status": status, "detail": detail,
	})
	obs.FromContext(ctx).InfoContext(ctx, "install completion reported",
		obs.FieldTaskID, task.ID, "status", status)
	return gen.ReportInstallComplete204Response{}, nil
}

// declaredStaticIP reads the job's resolved spec and returns the first
// declared static address of its network section. ok=false when the job
// carries no spec (discover/power), no network section, or every entry is
// DHCP-shaped — i.e. exactly the installs where the completion report's
// peer IS the machine's live address and should be recorded.
func (s *Server) declaredStaticIP(ctx context.Context, jobID string) (string, bool) {
	job, err := s.Jobs.GetJob(ctx, jobID)
	if err != nil || len(job.SpecResolved) == 0 {
		return "", false
	}
	var spec struct {
		Network []struct {
			Addresses []string `json:"addresses"`
		} `json:"network"`
	}
	if json.Unmarshal(job.SpecResolved, &spec) != nil {
		return "", false
	}
	for _, n := range spec.Network {
		for _, a := range n.Addresses {
			if ip := net.ParseIP(a); ip != nil && !ip.IsLoopback() {
				return ip.String(), true
			}
		}
	}
	return "", false
}

// ReportProbeFindings records the ramdisk probe's /sys scan: the body lands
// verbatim as a layout snapshot (source=ramdisk) and the event notifies any
// waiting discover task (docs/05-inventory.md §4).
func (s *Server) ReportProbeFindings(ctx context.Context, request gen.ReportProbeFindingsRequestObject) (gen.ReportProbeFindingsResponseObject, error) {
	task, err := s.Jobs.GetTaskByToken(ctx, request.Token)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(request.Body)
	if err != nil {
		return nil, verr("SCHEMA_INVALID_PROBE_REPORT", "probe report is not valid JSON content")
	}
	if _, err := s.Machines.SaveLayout(ctx, task.MachineID, "ramdisk", raw, 0); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "machine", task.MachineID, "machine.probe_reported", map[string]any{
		"task_id": task.ID,
	})
	obs.FromContext(ctx).InfoContext(ctx, "ramdisk probe report recorded",
		obs.FieldTaskID, task.ID, obs.FieldMachineID, task.MachineID)
	return gen.ReportProbeFindings204Response{}, nil
}

// uploadDiagMaxBytes caps one diagnostics upload (logs, not images).
const uploadDiagMaxBytes = 64 << 20

// UploadDiag accepts one diagnostics file from the installer environment
// (the windows startnet curls setup's Panther logs back after a failed
// launch). Machine-face credentialing: the unguessable token in the path.
// The file lands under <MediaDir>/diag/<token>/ for post-mortem.
func (s *Server) UploadDiag(c *gin.Context) {
	name := c.Param("name")
	if len(name) == 0 || len(name) > 64 {
		problem(c, http.StatusBadRequest, "SCHEMA_INVALID_DIAG_NAME", "Invalid name", "diag file name must be 1..64 chars", false)
		return
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			problem(c, http.StatusBadRequest, "SCHEMA_INVALID_DIAG_NAME", "Invalid name", "diag file name allows [A-Za-z0-9._-] only", false)
			return
		}
	}
	task, err := s.Jobs.GetTaskByToken(c.Request.Context(), c.Param("token"))
	if err != nil {
		writeError(c, err)
		return
	}
	if s.MediaDir == "" {
		problem(c, http.StatusServiceUnavailable, "DIAG_UNAVAILABLE", "Diagnostics unavailable", "media repo is not configured", false)
		return
	}
	dest := filepath.Join(s.MediaDir, "diag", task.ID, name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		problem(c, http.StatusInternalServerError, "DIAG_STORE_FAILED", "Diagnostics store failed", err.Error(), false)
		return
	}
	f, err := os.Create(dest)
	if err != nil {
		problem(c, http.StatusInternalServerError, "DIAG_STORE_FAILED", "Diagnostics store failed", err.Error(), false)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(c.Request.Body, uploadDiagMaxBytes)); err != nil {
		problem(c, http.StatusInternalServerError, "DIAG_STORE_FAILED", "Diagnostics store failed", err.Error(), false)
		return
	}
	obs.FromContext(c.Request.Context()).InfoContext(c.Request.Context(), "diagnostics uploaded",
		obs.FieldTaskID, task.ID, "file", name, "bytes", c.Request.ContentLength)
	c.Status(http.StatusNoContent)
}

// agentLogMaxBytes caps one agent-log ship — the agent tail-caps itself to
// 64KiB per beat (agentboot.go), and this is the server-side contract
// mirror; anything bigger is truncated, not rejected.
const agentLogMaxBytes = 64 << 10

// agentLogLineMax caps one filed line (an uncapped apk fetch progress line
// would otherwise dwarf the whole record).
const agentLogLineMax = 8 << 10

// UploadAgentLog files the agent runtime's log tail into the task log:
// body is plain text, one increment of [mammoth-agent] lines since the
// last ship, split and filed line by line the way the syslog sink files
// installer lines. Oversized ships are truncated, not rejected — the ship
// is best-effort on the machine side and must never wedge the install on
// a server-side size opinion.
func (s *Server) UploadAgentLog(c *gin.Context) {
	task, err := s.Jobs.GetTaskByToken(c.Request.Context(), c.Param("token"))
	if err != nil {
		writeError(c, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, agentLogMaxBytes+1))
	if err != nil {
		problem(c, http.StatusBadRequest, "SCHEMA_INVALID_AGENT_LOG", "Agent log rejected", err.Error(), false)
		return
	}
	if len(body) > agentLogMaxBytes {
		body = body[:agentLogMaxBytes]
	}
	ctx := c.Request.Context()
	shipped := 0
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if len(line) > agentLogLineMax {
			line = line[:agentLogLineMax]
		}
		s.Logs.AppendTaskLog(ctx, task.ID, "INFO", "agent", line, nil)
		shipped++
	}
	if shipped > 0 {
		obs.FromContext(ctx).InfoContext(ctx, "agent log shipped",
			obs.FieldTaskID, task.ID, "lines", shipped, "bytes", c.Request.ContentLength)
	}
	c.Status(http.StatusNoContent)
}

var _ = store.ErrNotFound
