package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/store"
)

// ── events: audit query + SSE streams (docs/03-api.md §4) ───────────────────
// The watermark is the monotonic events.id; SSE clients reconnect with
// Last-Event-ID and resume exactly where they left off.

func eventOut(e store.EventRecord) gen.Event {
	var payload *map[string]any
	if len(e.Payload) > 0 {
		var m map[string]any
		if json.Unmarshal(e.Payload, &m) == nil {
			payload = &m
		}
	}
	return gen.Event{
		Id:           e.ID,
		ResourceType: e.ResourceType,
		ResourceId:   e.ResourceID,
		Type:         e.Type,
		Payload:      payload,
		Ts:           e.TS,
	}
}

// ListEvents is the audit/backlog query surface (cursor over the watermark).
func (s *Server) ListEvents(ctx context.Context, request gen.ListEventsRequestObject) (gen.ListEventsResponseObject, error) {
	p := request.Params
	after := int64(0)
	if p.Cursor != nil && *p.Cursor != "" {
		v, err := strconv.ParseInt(string(*p.Cursor), 10, 64)
		if err != nil {
			return nil, verr("SCHEMA_INVALID_CURSOR", "cursor must be an event id")
		}
		after = v
	}
	// page_size defaults and clamps exactly like every other list endpoint
	// (contract: 1..200, default 50).
	pageSize := int(derefOr(p.PageSize, gen.PageSize(50)))
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 200 {
		pageSize = 200
	}
	events, err := s.Events.List(ctx, store.EventFilter{
		AfterID:      after,
		ResourceType: string(derefOr(p.ResourceType, gen.ListEventsParamsResourceType(""))),
		ResourceID:   derefOr(p.ResourceId, ""),
		Type:         derefOr(p.Type, ""),
		Limit:        pageSize + 1,
	})
	if err != nil {
		return nil, err
	}
	page := gen.EventList{Items: []gen.Event{}}
	for _, e := range events {
		page.Items = append(page.Items, eventOut(e))
	}
	if len(page.Items) > pageSize {
		page.Items = page.Items[:pageSize]
		next := strconv.FormatInt(page.Items[len(page.Items)-1].Id, 10)
		page.NextCursor = &next
	}
	return gen.ListEvents200JSONResponse(page), nil
}

// sseFrame is one server-sent event: a monotonic id, the event name, and
// the JSON payload.
type sseFrame struct {
	id    int64
	event string
	data  any
}

// ssePoll writes SSE frames to w, resuming after resumeID and polling fetch
// every second until ctx is done — the shared loop behind the event and
// task-log streams. eof, when non-nil, is consulted once the backlog has
// drained: returning true ends the stream with a terminal `eos` frame (the
// client's cue to stop reconnecting). Periodic keepalive comments keep
// intermediaries from reaping an idle connection. Poll cadence is
// fine-grained enough for operator UX at zero complexity cost (a
// LISTEN/NOTIFY upgrade path stays open behind the same interface).
func ssePoll(ctx context.Context, w http.ResponseWriter, resumeID int64,
	fetch func(ctx context.Context, after int64) ([]sseFrame, error),
	eof func(ctx context.Context) bool,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return verr("SCHEMA_SSE_UNSUPPORTED", "streaming unsupported by this connection")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 3000\n\n")
	flusher.Flush()

	last := resumeID
	idle := 0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		frames, err := fetch(ctx, last)
		if err != nil {
			return nil // client gone or transient; SSE contract: close silently
		}
		for _, f := range frames {
			data, _ := json.Marshal(f.data)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", f.id, f.event, data)
			last = f.id
		}
		if len(frames) > 0 {
			flusher.Flush()
			idle = 0
			continue
		}
		if eof != nil && eof(ctx) {
			fmt.Fprint(w, "event: eos\ndata: {}\n\n")
			flusher.Flush()
			return nil
		}
		if idle++; idle >= 15 {
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
			idle = 0
		}
	}
}

// sseStream streams events through the shared poll loop.
func (s *Server) sseStream(ctx context.Context, w http.ResponseWriter, filter store.EventFilter, resumeID int64) error {
	return ssePoll(ctx, w, resumeID, func(ctx context.Context, after int64) ([]sseFrame, error) {
		events, err := s.Events.List(ctx, store.EventFilter{
			AfterID:      after,
			ResourceType: filter.ResourceType,
			ResourceID:   filter.ResourceID,
			Type:         filter.Type,
			Limit:        200,
		})
		if err != nil {
			return nil, err
		}
		frames := make([]sseFrame, 0, len(events))
		for _, e := range events {
			frames = append(frames, sseFrame{id: e.ID, event: e.Type, data: eventOut(e)})
		}
		return frames, nil
	}, nil)
}

func resumeFrom(header *string) int64 {
	if header == nil || *header == "" {
		return 0
	}
	v, _ := strconv.ParseInt(*header, 10, 64)
	return v
}

// StreamJobEvents streams one job's events.
func (s *Server) StreamJobEvents(ctx context.Context, request gen.StreamJobEventsRequestObject) (gen.StreamJobEventsResponseObject, error) {
	if _, err := s.Jobs.GetJob(ctx, string(request.Id)); err != nil {
		return nil, err
	}
	resume := resumeFrom(request.Params.LastEventID)
	return &jobEventStream{server: s, ctx: ctx, jobID: string(request.Id), resume: resume}, nil
}

type jobEventStream struct {
	server *Server
	ctx    context.Context
	jobID  string
	resume int64
}

func (j *jobEventStream) VisitStreamJobEventsResponse(w http.ResponseWriter) error {
	return j.server.sseStream(j.ctx, w, store.EventFilter{
		ResourceType: "job",
		ResourceID:   j.jobID,
	}, j.resume)
}

// StreamEvents streams all events, optionally filtered to one resource.
func (s *Server) StreamEvents(ctx context.Context, request gen.StreamEventsRequestObject) (gen.StreamEventsResponseObject, error) {
	resume := resumeFrom(request.Params.LastEventID)
	return &globalEventStream{server: s, ctx: ctx, resource: derefOr(request.Params.Resource, ""), resume: resume}, nil
}

type globalEventStream struct {
	server   *Server
	ctx      context.Context
	resource string
	resume   int64
}

func (g *globalEventStream) VisitStreamEventsResponse(w http.ResponseWriter) error {
	return g.server.sseStream(g.ctx, w, store.EventFilter{
		ResourceID: g.resource,
	}, g.resume)
}
