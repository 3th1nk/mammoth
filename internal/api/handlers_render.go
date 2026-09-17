package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
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

var _ = store.ErrNotFound
