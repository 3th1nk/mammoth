package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// ── task logs: the persisted half of the log dual-write (docs/03-api.md) ────
// events answer "what happened to this task"; task_logs answer "what did the
// runner do and observe". Both are cursor-paginated over their monotonic id.

func taskLogOut(l store.TaskLogRecord) gen.TaskLog {
	var attrs *map[string]any
	if len(l.Attrs) > 0 {
		var m map[string]any
		if json.Unmarshal(l.Attrs, &m) == nil && len(m) > 0 {
			attrs = &m
		}
	}
	var stage *string
	if l.Stage != "" {
		s := l.Stage
		stage = &s
	}
	return gen.TaskLog{
		Id:      l.ID,
		Level:   gen.TaskLogLevel(l.Level),
		Stage:   stage,
		Message: l.Message,
		Attrs:   attrs,
		Ts:      l.TS,
	}
}

// StreamJobTaskLogs tails one task's execution log as SSE (docs/03-api.md):
// backlog replay after the resume point, live lines as they land, and a
// terminal `eos` frame once the task is terminal (succeeded/failed/canceled)
// and the backlog has drained. interrupted stays open — a retry resumes the
// same task record and the stream simply picks the new lines up.
func (s *Server) StreamJobTaskLogs(ctx context.Context, request gen.StreamJobTaskLogsRequestObject) (gen.StreamJobTaskLogsResponseObject, error) {
	if _, err := s.Jobs.GetJob(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	task, err := s.Jobs.GetTask(ctx, string(request.TaskId))
	if err != nil {
		return nil, err
	}
	if task.JobID != string(request.Id) {
		return nil, store.ErrNotFound
	}
	// Last-Event-ID wins over `after`: the reconnect header carries the
	// exact watermark the client actually saw.
	resume := resumeFrom(request.Params.LastEventID)
	if resume == 0 && request.Params.After != nil && *request.Params.After != "" {
		v, err := strconv.ParseInt(*request.Params.After, 10, 64)
		if err != nil {
			return nil, verr("SCHEMA_INVALID_CURSOR", "after must be a task log id")
		}
		resume = v
	}
	return &taskLogStream{server: s, ctx: ctx, taskID: task.ID, resume: resume}, nil
}

type taskLogStream struct {
	server *Server
	ctx    context.Context
	taskID string
	resume int64
}

func (t *taskLogStream) VisitStreamJobTaskLogsResponse(w http.ResponseWriter) error {
	return ssePoll(t.ctx, w, t.resume, func(ctx context.Context, after int64) ([]sseFrame, error) {
		logs, err := t.server.Logs.List(ctx, store.TaskLogFilter{
			TaskID:  t.taskID,
			AfterID: after,
			Limit:   200,
		})
		if err != nil {
			return nil, err
		}
		frames := make([]sseFrame, 0, len(logs))
		for _, l := range logs {
			frames = append(frames, sseFrame{id: l.ID, event: "log", data: taskLogOut(l)})
		}
		return frames, nil
	}, func(ctx context.Context) bool {
		task, err := t.server.Jobs.GetTask(ctx, t.taskID)
		if err != nil {
			return false
		}
		switch task.State {
		case "succeeded", "failed", "canceled":
			return true
		}
		return false
	})
}

// ListJobTaskLogs returns one task's execution trail in write order. The
// task must belong to the path job (same rule as getJobTask).
func (s *Server) ListJobTaskLogs(ctx context.Context, request gen.ListJobTaskLogsRequestObject) (gen.ListJobTaskLogsResponseObject, error) {
	task, err := s.Jobs.GetTask(ctx, string(request.TaskId))
	if err != nil {
		return nil, err
	}
	if task.JobID != string(request.Id) {
		return nil, store.ErrNotFound
	}
	after := int64(0)
	if p := request.Params.Cursor; p != nil && *p != "" {
		v, err := strconv.ParseInt(string(*p), 10, 64)
		if err != nil {
			return nil, verr("SCHEMA_INVALID_CURSOR", "cursor must be a task log id")
		}
		after = v
	}
	// page_size defaults and clamps exactly like every other list endpoint
	// (contract: 1..200, default 50).
	pageSize := int(derefOr(request.Params.PageSize, gen.PageSize(50)))
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 200 {
		pageSize = 200
	}
	logs, err := s.Logs.List(ctx, store.TaskLogFilter{
		TaskID:  task.ID,
		AfterID: after,
		Limit:   pageSize + 1,
	})
	if err != nil {
		return nil, err
	}
	page := gen.TaskLogList{Items: []gen.TaskLog{}}
	for _, l := range logs {
		page.Items = append(page.Items, taskLogOut(l))
	}
	if len(page.Items) > pageSize {
		page.Items = page.Items[:pageSize]
		next := strconv.FormatInt(page.Items[len(page.Items)-1].Id, 10)
		page.NextCursor = &next
	}
	return gen.ListJobTaskLogs200JSONResponse(page), nil
}
