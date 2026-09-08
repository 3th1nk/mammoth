package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/3th1nk/mammoth/internal/api/gen"
	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// ── webhooks: event-delivery subscriptions (docs/09-roadmap.md M6) ──────────
// The signing secret is write-only: server-generated when omitted, shown
// exactly once in the creation response, never returned afterwards.

func (s *Server) CreateWebhook(ctx context.Context, request gen.CreateWebhookRequestObject) (gen.CreateWebhookResponseObject, error) {
	body := request.Body
	if body == nil || body.Url == "" {
		return nil, verr("SCHEMA_INVALID_WEBHOOK", "url is required")
	}
	secret := derefOr(body.Secret, "")
	if secret == "" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, fmt.Errorf("secret entropy: %w", err)
		}
		secret = hex.EncodeToString(b[:])
	}
	sealed, err := s.Crypto.Encrypt([]byte(secret))
	if err != nil {
		return nil, err
	}

	wh := &store.Webhook{
		ID:         store.NewID("wh"),
		URL:        body.Url,
		SecretEnc:  sealed,
		Types:      derefOr(body.Types, nil),
		ResourceID: derefOr(body.ResourceId, ""),
		Enabled:    true,
	}
	if err := s.Webhooks.Create(ctx, wh); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "webhook", wh.ID, "webhook.created", map[string]any{"url": wh.URL})
	obs.FromContext(ctx).InfoContext(ctx, "webhook created", "webhook_id", wh.ID)

	out := gen.WebhookCreated{
		Enabled:    wh.Enabled,
		Id:         wh.ID,
		ResourceId: resourceIdPtr(wh.ResourceID),
		Types:      typesPtr(wh.Types),
		Url:        wh.URL,
		Secret:     secret,
	}
	return gen.CreateWebhook201JSONResponse(out), nil
}

func (s *Server) ListWebhooks(ctx context.Context, _ gen.ListWebhooksRequestObject) (gen.ListWebhooksResponseObject, error) {
	items, err := s.Webhooks.List(ctx)
	if err != nil {
		return nil, err
	}
	out := gen.WebhookList{Items: []gen.Webhook{}}
	for _, w := range items {
		out.Items = append(out.Items, webhookOut(w))
	}
	return gen.ListWebhooks200JSONResponse(out), nil
}

func (s *Server) GetWebhook(ctx context.Context, request gen.GetWebhookRequestObject) (gen.GetWebhookResponseObject, error) {
	w, err := s.Webhooks.Get(ctx, request.Id)
	if err != nil {
		return nil, err
	}
	return gen.GetWebhook200JSONResponse(webhookOut(w)), nil
}

func (s *Server) DeleteWebhook(ctx context.Context, request gen.DeleteWebhookRequestObject) (gen.DeleteWebhookResponseObject, error) {
	if err := s.Webhooks.Delete(ctx, request.Id); err != nil {
		return nil, err
	}
	s.Events.Append(ctx, "webhook", request.Id, "webhook.deleted", nil)
	return gen.DeleteWebhook204Response{}, nil
}

func webhookOut(w *store.Webhook) gen.Webhook {
	var rid *string
	if w.ResourceID != "" {
		rid = str(w.ResourceID)
	}
	failCount := w.FailCount
	watermark := w.Watermark
	return gen.Webhook{
		Id:         w.ID,
		Url:        w.URL,
		Types:      typesPtr(w.Types),
		ResourceId: rid,
		Enabled:    w.Enabled,
		Watermark:  &watermark,
		FailCount:  &failCount,
		CreatedAt:  w.CreatedAt,
	}
}

func resourceIdPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func typesPtr(t []string) *[]string {
	if t == nil {
		return nil
	}
	return &t
}
