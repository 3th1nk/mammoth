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

// maxTailParam caps the stream `tail` bootstrap — the contract mirror of
// the largest page the list endpoint serves (200).
const maxTailParam = 200

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
	// `tail` bounds a FRESH subscription's replay (no explicit resume point):
	// resume after newest-tail instead of the task's first line. An install
	// log is an hours-long trail (syslog sink files every installer journal
	// line) — a viewer opening the page wants the tail rendered instantly,
	// not a 200-lines/s crawl of the whole history. Contract clamps to
	// 0..200 like page_size.
	if resume == 0 && request.Params.Tail != nil {
		tail := *request.Params.Tail
		if tail < 0 {
			tail = 0
		}
		if tail > maxTailParam {
			tail = maxTailParam
		}
		if tail > 0 {
			last, err := s.Logs.LastID(ctx, task.ID)
			if err != nil {
				return nil, err
			}
			if last > int64(tail) {
				resume = last - int64(tail)
			}
		}
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

// ListJobTaskLogs returns one task's execution trail, asc (default) in
// write order or desc newest-first (the load-older direction). The task
// must belong to the path job (same rule as getJobTask).
func (s *Server) ListJobTaskLogs(ctx context.Context, request gen.ListJobTaskLogsRequestObject) (gen.ListJobTaskLogsResponseObject, error) {
	task, err := s.Jobs.GetTask(ctx, string(request.TaskId))
	if err != nil {
		return nil, err
	}
	if task.JobID != string(request.Id) {
		return nil, store.ErrNotFound
	}
	// Cursor means "continue from here" in the ACTIVE direction: asc pages
	// forward after it, desc pages backward before it — one opaque token
	// serves both, the client just passes it back with the same order.
	var cursor int64
	if p := request.Params.Cursor; p != nil && *p != "" {
		v, err := strconv.ParseInt(string(*p), 10, 64)
		if err != nil {
			return nil, verr("SCHEMA_INVALID_CURSOR", "cursor must be a task log id")
		}
		cursor = v
	}
	desc := derefOr(request.Params.Order, gen.ListJobTaskLogsParamsOrderAsc) == gen.ListJobTaskLogsParamsOrderDesc
	// page_size defaults and clamps exactly like every other list endpoint
	// (contract: 1..200, default 50).
	pageSize := int(derefOr(request.Params.PageSize, gen.PageSize(50)))
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 200 {
		pageSize = 200
	}
	filter := store.TaskLogFilter{TaskID: task.ID, Desc: desc, Limit: pageSize + 1}
	if desc {
		filter.BeforeID = cursor
	} else {
		filter.AfterID = cursor
	}
	logs, err := s.Logs.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	page := gen.TaskLogList{Items: []gen.TaskLog{}}
	for _, l := range logs {
		page.Items = append(page.Items, taskLogOut(l))
	}
	if len(page.Items) > pageSize {
		page.Items = page.Items[:pageSize]
		// The trimmed page's last element is the continuation point in both
		// directions: asc → the page's largest id (continue after it),
		// desc → the page's smallest id (continue before it).
		next := strconv.FormatInt(page.Items[len(page.Items)-1].Id, 10)
		page.NextCursor = &next
	}
	return gen.ListJobTaskLogs200JSONResponse(page), nil
}
