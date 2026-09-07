package api

import (
	"context"
	"encoding/json"
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
	s.Events.Append(ctx, "task", task.ID, "task.install_reported", map[string]any{
		"status": status, "detail": detail,
	})
	obs.FromContext(ctx).InfoContext(ctx, "install completion reported",
		obs.FieldTaskID, task.ID, "status", status)
	return gen.ReportInstallComplete204Response{}, nil
}

var _ = store.ErrNotFound
