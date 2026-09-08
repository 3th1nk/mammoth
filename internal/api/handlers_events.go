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
	events, err := s.Events.List(ctx, store.EventFilter{
		AfterID:      after,
		ResourceType: string(derefOr(p.ResourceType, gen.ListEventsParamsResourceType(""))),
		ResourceID:   derefOr(p.ResourceId, ""),
		Type:         derefOr(p.Type, ""),
		Limit:        int(derefOr(p.PageSize, 100)) + 1,
	})
	if err != nil {
		return nil, err
	}
	page := gen.EventList{Items: []gen.Event{}}
	for _, e := range events {
		page.Items = append(page.Items, eventOut(e))
	}
	if len(page.Items) > int(derefOr(p.PageSize, 100)) {
		page.Items = page.Items[:int(derefOr(p.PageSize, 100))]
		next := strconv.FormatInt(page.Items[len(page.Items)-1].Id, 10)
		page.NextCursor = &next
	}
	return gen.ListEvents200JSONResponse(page), nil
}

// sseStream writes events to w starting after resumeID until ctx is done.
// Poll cadence is fine-grained enough for operator UX at zero complexity cost
// (a LISTEN/NOTIFY upgrade path stays open behind the same interface).
func (s *Server) sseStream(ctx context.Context, w http.ResponseWriter, filter store.EventFilter, resumeID int64) error {
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		events, err := s.Events.List(ctx, store.EventFilter{
			AfterID:      last,
			ResourceType: filter.ResourceType,
			ResourceID:   filter.ResourceID,
			Type:         filter.Type,
			Limit:        200,
		})
		if err != nil {
			return nil // client gone or transient; SSE contract: close silently
		}
		for _, e := range events {
			data, _ := json.Marshal(eventOut(e))
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, data)
			last = e.ID
		}
		if len(events) > 0 {
			flusher.Flush()
		}
	}
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
