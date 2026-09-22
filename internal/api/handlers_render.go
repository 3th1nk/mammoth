package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

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
	// recorded address untouched.
	if ip := net.ParseIP(ClientIPFromContext(ctx)); ip != nil && ip.IsPrivate() && !ip.IsLoopback() {
		if uerr := s.Machines.SetSSHAddress(ctx, task.MachineID, ip.String()); uerr == nil {
			obs.FromContext(ctx).InfoContext(ctx, "machine ssh address refreshed from completion report",
				obs.FieldMachineID, task.MachineID, "ssh_address", ip.String())
		}
	}
	s.Events.Append(ctx, "task", task.ID, "task.install_reported", map[string]any{
		"status": status, "detail": detail,
	})
	obs.FromContext(ctx).InfoContext(ctx, "install completion reported",
		obs.FieldTaskID, task.ID, "status", status)
	return gen.ReportInstallComplete204Response{}, nil
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

var _ = store.ErrNotFound
