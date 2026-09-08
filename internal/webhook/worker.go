// Package webhook delivers events to subscribed HTTP endpoints
// (docs/09-roadmap.md M6): at-least-once delivery with HMAC-SHA256 signing,
// per-subscription watermark, and exponential failure backoff.
//
// Delivery contract (documented in api/openapi.yaml):
//   - POST to the subscription URL with the event JSON body;
//   - headers: X-Mammoth-Event-Id, X-Mammoth-Event-Type,
//     X-Mammoth-Signature: sha256=<hex(hmac-sha256(secret, body))>;
//   - 2xx advances the watermark; anything else increments the failure
//     streak and retries the same events with backoff.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/3th1nk/mammoth/internal/obs"
	"github.com/3th1nk/mammoth/internal/store"
)

// Options size the delivery worker.
type Options struct {
	Interval       time.Duration // sweep cadence (default 2s); also the backoff unit
	Timeout        time.Duration // per-delivery HTTP timeout (default 10s)
	BatchLimit     int           // events per subscription per sweep (default 50)
	MaxFailBackoff int           // backoff exponent cap (default 6 → ≤64×interval)
}

// Worker delivers events to enabled subscriptions.
type Worker struct {
	Repo   *store.WebhookRepo
	Events *store.EventRepo
	Crypto *store.SecretCrypto
	Opts   Options
	Logger *slog.Logger
	HTTP   *http.Client

	nextAttempt map[string]time.Time // webhook id → earliest next attempt (in-memory backoff)
}

// Run blocks until ctx is canceled.
func (w *Worker) Run(ctx context.Context) error {
	w.nextAttempt = map[string]time.Time{}
	interval := w.Opts.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timeout := w.Opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	batch := w.Opts.BatchLimit
	if batch <= 0 {
		batch = 50
	}
	maxGap := w.Opts.MaxFailBackoff
	if maxGap <= 0 {
		maxGap = 6
	}
	if w.HTTP == nil {
		w.HTTP = &http.Client{Timeout: timeout}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		w.sweep(ctx, batch, maxGap)
	}
}

func (w *Worker) sweep(ctx context.Context, batch, maxGap int) {
	subs, err := w.Repo.List(ctx)
	if err != nil {
		obs.FromContext(ctx).ErrorContext(ctx, "webhook sweep: list failed", "err", err.Error())
		return
	}
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		// Exponential backoff: a failure schedules the next attempt at
		// 2^fail_count × interval (capped), in memory — delivery state that
		// survives only this worker is safe to lose.
		if until, ok := w.nextAttempt[sub.ID]; ok && time.Now().Before(until) {
			continue
		}
		w.deliver(ctx, sub, batch, maxGap)
	}
}

func (w *Worker) deliver(ctx context.Context, sub *store.Webhook, batch, maxGap int) {
	events, err := w.Events.List(ctx, store.EventFilter{
		AfterID:    sub.Watermark,
		ResourceID: sub.ResourceID,
		Limit:      batch,
	})
	if err != nil {
		obs.FromContext(ctx).ErrorContext(ctx, "webhook sweep: events failed",
			obs.FieldQueue, sub.ID, "err", err.Error())
		return
	}
	if len(events) == 0 {
		return
	}

	plain, err := w.Crypto.Decrypt(sub.SecretEnc)
	if err != nil {
		obs.FromContext(ctx).ErrorContext(ctx, "webhook secret decrypt failed",
			"webhook_id", sub.ID, "err", err.Error())
		return
	}

	for _, e := range events {
		// Type filter: deliver only subscribed types, but the watermark
		// advances past skipped events (they are not coming back).
		if len(sub.Types) > 0 && !typeMatches(sub.Types, e.Type) {
			_ = w.Repo.AdvanceDelivery(ctx, sub.ID, e.ID)
			continue
		}
		if err := w.post(ctx, sub, plain, e); err != nil {
			_ = w.Repo.RecordDeliveryFailure(ctx, sub.ID)
			fails := sub.FailCount + 1
			gap := fails
			if gap > maxGap {
				gap = maxGap
			}
			if w.nextAttempt == nil {
				w.nextAttempt = map[string]time.Time{}
			}
			w.nextAttempt[sub.ID] = time.Now().Add(w.Opts.Interval * time.Duration(1<<uint(gap)))
			obs.FromContext(ctx).WarnContext(ctx, "webhook delivery failed",
				"webhook_id", sub.ID, "event_id", e.ID, "err", err.Error())
			return // watermark stays; retry next sweep in order
		}
		if err := w.Repo.AdvanceDelivery(ctx, sub.ID, e.ID); err != nil {
			obs.FromContext(ctx).ErrorContext(ctx, "webhook watermark advance failed",
				"webhook_id", sub.ID, "err", err.Error())
			return
		}
		delete(w.nextAttempt, sub.ID)
	}
}

func (w *Worker) post(ctx context.Context, sub *store.Webhook, secret []byte, e store.EventRecord) error {
	body, err := json.Marshal(store.EventRecord{
		ID: e.ID, ResourceType: e.ResourceType, ResourceID: e.ResourceID,
		Type: e.Type, Payload: e.Payload, TS: e.TS,
	})
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mammoth-Event-Id", fmt.Sprint(e.ID))
	req.Header.Set("X-Mammoth-Event-Type", e.Type)
	req.Header.Set("X-Mammoth-Signature", sig)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook endpoint returned %s", resp.Status)
	}
	return nil
}

func typeMatches(types []string, typ string) bool {
	for _, t := range types {
		if t == typ {
			return true
		}
		// prefix wildcard support: "machine.*" delivers all machine events
		if strings.HasSuffix(t, "*") && strings.HasPrefix(typ, strings.TrimSuffix(t, "*")) {
			return true
		}
	}
	return false
}
